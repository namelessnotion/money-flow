package main

import (
	"context"
	"testing"
	"time"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/holder"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
)

// integrationWait is generous against an in-process orchestrator that
// normally catches up within a millisecond or two. It only needs to outlast
// the slowest delivery, and a run that converges never spends it.
var integrationWait = settleWait{attempts: 2000, delay: time.Millisecond}

// simulateWorld is the whole system in-process: the real
// Holder/Transaction/Transfer servers over a MemoryStore and a fake
// TigerBeetle, the same wiring saga.Wire gives cmd/server, with the
// orchestrator consuming published triggers in the background as
// cmd/orchestrator does. Nothing the tool calls drives a saga; only that
// background delivery does.
type simulateWorld struct {
	store   *publishingStore
	servers saga.Servers
	holders *holder.Server
}

func newSimulateWorld(t *testing.T) simulateWorld {
	t.Helper()
	store := newPublishingStore(eventstore.NewMemoryStore())
	servers := saga.Wire(store, ledger.NewFakeClient())
	startOrchestrator(t, store, servers.Orchestrator())
	return simulateWorld{store: store, servers: servers, holders: holder.NewServer(store)}
}

func (w simulateWorld) leg() legReader {
	return storeLegReader(w.store)
}

const (
	// numEntities is kept well above concurrency so pickPair rarely
	// double-books the same Wallet across concurrent workers: a Transfer
	// whose prepare step loses a concurrency race on a hot Wallet is left
	// mid-flight until a later trigger moves it.
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

// provisionAndSeed runs the tool's own setup against w and hands back the
// entities and their seeded balances. seedAll only returns once every seed
// has actually committed, so the load that follows can spend it.
func provisionAndSeed(t *testing.T, w simulateWorld) ([]entity, map[string]int64) {
	t.Helper()
	ctx := context.Background()

	reserve, entities, err := provisionAll(ctx, w.holders, numEntities)
	if err != nil {
		t.Fatalf("provisionAll: %v", err)
	}
	if err := seedAll(ctx, w.servers.Transaction, reserve, entities, initialBalance, currency, integrationWait); err != nil {
		t.Fatalf("seedAll: %v", err)
	}

	initial := make(map[string]int64, len(entities))
	for _, e := range entities {
		initial[e.walletID] = initialBalance
	}
	return entities, initial
}

// TestSimulateEndToEnd runs the whole tool — provision, seed, load, verify —
// against the real sagas. It exists to prove this package's understanding of
// them against the actual implementation rather than a hand-written fake: a
// staged leg is settled only once it has actually staged, and the Transaction
// is read until the orchestrator has folded whatever that settlement caused.
func TestSimulateEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newSimulateWorld(t)
	entities, initial := provisionAndSeed(t, w)

	d := transactionDriver{
		transactions: w.servers.Transaction, transfers: w.servers.Transfer, leg: w.leg(), wait: integrationWait,
	}
	cfg := runConfig{currency: currency, minAmount: minAmount, maxAmount: maxAmount, rollbackRate: rollbackRate}
	results, wallClock := runLoad(ctx, entities, cfg, numTransactions, concurrency, seed, d.drive)
	if wallClock <= 0 {
		t.Error("runLoad: wallClock = 0, want a positive duration")
	}

	completed, rolledBack, open := 0, 0, 0
	for i, r := range results {
		if r.err != nil {
			t.Errorf("result[%d]: unexpected transport error: %v", i, r.err)
			continue
		}
		switch {
		case r.open:
			// Lost a concurrency race on a hot Wallet and was not moved again
			// within the wait — legitimate under contention, just not the
			// common case at these entity counts.
			open++
		case r.final == "TRANSACTION_STATE_COMPLETED":
			completed++
		case r.final == "TRANSACTION_STATE_ROLLED_BACK":
			rolledBack++
		default:
			t.Errorf("result[%d]: final state = %s (%s), want COMPLETED, ROLLED_BACK, or still open", i, r.final, r.reason)
		}
	}
	if completed == 0 || rolledBack == 0 {
		t.Errorf("completed=%d rolledBack=%d, want both to have occurred across %d transactions with rollback-rate %.2f",
			completed, rolledBack, numTransactions, rollbackRate)
	}
	if open > numTransactions/10 {
		t.Errorf("open=%d of %d transactions, want at most 10%% — unexpectedly high contention", open, numTransactions)
	}

	assertConserved(t, w, entities, initial, results)
}

// TestSimulateEndToEndTransferMode is TestSimulateEndToEnd's -mode=transfer
// counterpart: the same provision/seed/load/verify run, but requesting bare
// Transfers with no owning Transaction.
func TestSimulateEndToEndTransferMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newSimulateWorld(t)
	entities, initial := provisionAndSeed(t, w)

	d := transferDriver{transfers: w.servers.Transfer, leg: w.leg(), wait: integrationWait}
	cfg := runConfig{currency: currency, minAmount: minAmount, maxAmount: maxAmount, rollbackRate: rollbackRate}
	results, wallClock := runLoad(ctx, entities, cfg, numTransactions, concurrency, seed, d.drive)
	if wallClock <= 0 {
		t.Error("runLoad: wallClock = 0, want a positive duration")
	}

	committed, cancelled, open := 0, 0, 0
	for i, r := range results {
		if r.err != nil {
			t.Errorf("result[%d]: unexpected transport error: %v", i, r.err)
			continue
		}
		switch {
		case r.open:
			open++
		case r.final == "TRANSFER_COMMITTED":
			committed++
		case r.final == "TRANSFER_CANCELLED":
			cancelled++
		default:
			t.Errorf("result[%d]: final state = %s (%s), want TRANSFER_COMMITTED, TRANSFER_CANCELLED, or still open", i, r.final, r.reason)
		}
	}
	if committed == 0 || cancelled == 0 {
		t.Errorf("committed=%d cancelled=%d, want both to have occurred across %d transfers with rollback-rate %.2f",
			committed, cancelled, numTransactions, rollbackRate)
	}
	if open > numTransactions/10 {
		t.Errorf("open=%d of %d transfers, want at most 10%% — unexpectedly high contention", open, numTransactions)
	}

	assertConserved(t, w, entities, initial, results)
}

// assertConserved runs the tool's own post-run verification and fails on any
// entity whose balance does not match what the results say moved.
func assertConserved(t *testing.T, w simulateWorld, entities []entity, initial map[string]int64, results []txResult) {
	t.Helper()
	expected := computeExpected(initial, results)
	reconciliations, err := reconcile(context.Background(), w.store, entities, expected)
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
