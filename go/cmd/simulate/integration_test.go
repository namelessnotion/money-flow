package main

import (
	"context"
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/holder"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
)

// TestSimulateEndToEnd runs the whole tool — provision, seed, load, verify
// — against the real Holder/Transaction/Transfer sagas wired in-process
// over a MemoryStore and a fake TigerBeetle, the same wiring saga.Wire gives
// cmd/server, just without a real database or ledger. It exists to prove
// this package's understanding of the saga (in particular: a staged
// Transfer's Transaction stays Started until settled or rolled back, and
// ResumeTransaction is what notices either) against the actual
// implementation, not a hand-written fake of it.
func TestSimulateEndToEnd(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := eventstore.NewMemoryStore()
	servers := saga.Wire(store, ledger.NewFakeClient())
	holders := holder.NewServer(store)

	const (
		// numEntities is kept well above concurrency so pickPair rarely
		// double-books the same Wallet across concurrent workers: a Transfer
		// whose prepare step loses a concurrency race on a hot Wallet is left
		// mid-flight until something resumes its saga (see run.go's settle),
		// which nothing in this in-process test does.
		numEntities            = 30
		numTransactions        = 90
		concurrency            = 8
		initialBalance         = 100_000
		minAmount              = 100
		maxAmount              = 5_000
		rollbackRate           = 0.4
		currency               = "USD"
		seed            uint64 = 42
	)

	reserve, entities, err := provisionAll(ctx, holders, numEntities)
	if err != nil {
		t.Fatalf("provisionAll: %v", err)
	}

	initial := make(map[string]int64, len(entities))
	for _, e := range entities {
		if err := seedEntity(ctx, servers.Transaction, reserve, e, initialBalance, currency); err != nil {
			t.Fatalf("seedEntity(%s): %v", e.name, err)
		}
		initial[e.walletID] = initialBalance
	}

	drive := func(ctx context.Context, from, to entity, amount uint64, currency string, planned outcome) txResult {
		return driveOne(ctx, servers.Transaction, servers.Transfer, from, to, amount, currency, planned)
	}
	cfg := runConfig{currency: currency, minAmount: minAmount, maxAmount: maxAmount, rollbackRate: rollbackRate}
	results, wallClock := runLoad(ctx, entities, cfg, numTransactions, concurrency, seed, drive)
	if wallClock <= 0 {
		t.Error("runLoad: wallClock = 0, want a positive duration")
	}

	completed, rolledBack, stuck := 0, 0, 0
	for i, r := range results {
		if r.err != nil {
			t.Errorf("result[%d]: unexpected transport error: %v", i, r.err)
			continue
		}
		switch r.final {
		case "TRANSACTION_STATE_COMPLETED":
			completed++
		case "TRANSACTION_STATE_ROLLED_BACK":
			rolledBack++
		case "TRANSACTION_STATE_STARTED":
			// A Transfer that lost a concurrency race on a hot Wallet and
			// never reached Staged (see run.go's settle) — legitimate under
			// contention, just not the common case at these entity counts.
			stuck++
		default:
			t.Errorf("result[%d]: final state = %s, want COMPLETED, ROLLED_BACK, or STARTED", i, r.final)
		}
	}
	if completed == 0 || rolledBack == 0 {
		t.Errorf("completed=%d rolledBack=%d, want both to have occurred across %d transactions with rollback-rate %.2f",
			completed, rolledBack, numTransactions, rollbackRate)
	}
	if stuck > numTransactions/10 {
		t.Errorf("stuck=%d of %d transactions, want at most 10%% — unexpectedly high contention", stuck, numTransactions)
	}

	expected := computeExpected(initial, results)
	reconciliations, err := reconcile(ctx, store, entities, expected)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var expectedTotal, actualTotal int64
	for _, r := range reconciliations {
		expectedTotal += r.expected
		actualTotal += r.actual
		if !r.ok() {
			t.Errorf("entity %q: expected balance %d, actual %d (diff %d)", r.name, r.expected, r.actual, r.actual-r.expected)
		}
	}
	if actualTotal != expectedTotal {
		t.Errorf("conservation: actual total %d != expected total %d", actualTotal, expectedTotal)
	}
	if wantTotal := int64(numEntities) * initialBalance; actualTotal != wantTotal {
		t.Errorf("conservation: actual total %d != total seeded %d (entity-to-entity transfers must be zero-sum)", actualTotal, wantTotal)
	}
}

// TestSimulateEndToEndTransferMode is TestSimulateEndToEnd's -mode=transfer
// counterpart: the same provision/seed/load/verify run, but driving
// TransferService.RequestTransfer directly with no owning Transaction, to
// prove driveOneTransfer/settleTransfer's understanding of a bare Transfer's
// own saga against the real implementation.
func TestSimulateEndToEndTransferMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := eventstore.NewMemoryStore()
	servers := saga.Wire(store, ledger.NewFakeClient())
	holders := holder.NewServer(store)

	const (
		numEntities            = 30
		numTransactions        = 90
		concurrency            = 8
		initialBalance         = 100_000
		minAmount              = 100
		maxAmount              = 5_000
		rollbackRate           = 0.4
		currency               = "USD"
		seed            uint64 = 42
	)

	reserve, entities, err := provisionAll(ctx, holders, numEntities)
	if err != nil {
		t.Fatalf("provisionAll: %v", err)
	}

	initial := make(map[string]int64, len(entities))
	for _, e := range entities {
		if err := seedEntity(ctx, servers.Transaction, reserve, e, initialBalance, currency); err != nil {
			t.Fatalf("seedEntity(%s): %v", e.name, err)
		}
		initial[e.walletID] = initialBalance
	}

	drive := func(ctx context.Context, from, to entity, amount uint64, currency string, planned outcome) txResult {
		return driveOneTransfer(ctx, servers.Transfer, from, to, amount, currency, planned)
	}
	cfg := runConfig{currency: currency, minAmount: minAmount, maxAmount: maxAmount, rollbackRate: rollbackRate}
	results, wallClock := runLoad(ctx, entities, cfg, numTransactions, concurrency, seed, drive)
	if wallClock <= 0 {
		t.Error("runLoad: wallClock = 0, want a positive duration")
	}

	committed, cancelled, stuck := 0, 0, 0
	for i, r := range results {
		if r.err != nil {
			t.Errorf("result[%d]: unexpected transport error: %v", i, r.err)
			continue
		}
		switch r.final {
		case "TRANSFER_COMMITTED":
			committed++
		case "TRANSFER_CANCELLED":
			cancelled++
		case "TRANSFER_ACCEPTED", "TRANSFER_PENDING":
			// Lost a concurrency race on a hot Wallet and never reached
			// Staged (see run.go's settleTransfer) — legitimate under
			// contention, just not the common case at these entity counts.
			stuck++
		default:
			t.Errorf("result[%d]: final state = %s, want TRANSFER_COMMITTED, TRANSFER_CANCELLED, TRANSFER_ACCEPTED, or TRANSFER_PENDING", i, r.final)
		}
	}
	if committed == 0 || cancelled == 0 {
		t.Errorf("committed=%d cancelled=%d, want both to have occurred across %d transactions with rollback-rate %.2f",
			committed, cancelled, numTransactions, rollbackRate)
	}
	if stuck > numTransactions/10 {
		t.Errorf("stuck=%d of %d transactions, want at most 10%% — unexpectedly high contention", stuck, numTransactions)
	}

	expected := computeExpected(initial, results)
	reconciliations, err := reconcile(ctx, store, entities, expected)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var expectedTotal, actualTotal int64
	for _, r := range reconciliations {
		expectedTotal += r.expected
		actualTotal += r.actual
		if !r.ok() {
			t.Errorf("entity %q: expected balance %d, actual %d (diff %d)", r.name, r.expected, r.actual, r.actual-r.expected)
		}
	}
	if actualTotal != expectedTotal {
		t.Errorf("conservation: actual total %d != expected total %d", actualTotal, expectedTotal)
	}
	if wantTotal := int64(numEntities) * initialBalance; actualTotal != wantTotal {
		t.Errorf("conservation: actual total %d != total seeded %d (entity-to-entity transfers must be zero-sum)", actualTotal, wantTotal)
	}
}
