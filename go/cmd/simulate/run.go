package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"uuid"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// runConfig is everything one simulated transaction needs to pick its
// amount and outcome, independent of which worker runs it.
type runConfig struct {
	currency     string
	minAmount    uint64
	maxAmount    uint64
	rollbackRate float64
}

// simulatedRollover is the reason every planned rollback gives, so a report
// can tell a steered rollback from one the ledger decided on its own.
const simulatedRollover = "simulated rollover"

// seedAll mints amount fresh into every entity's wallet from reserve, one
// mint_source Transaction each — the simulated equivalent of an ACH deposit's
// shadow leg recognizing new cash in — and returns only once every one has
// committed. Accepting a seed funds nothing, and the load that follows spends
// what the seeds put there.
//
// Seeds settle batch at a time: every seed in a batch is started before any
// is waited on, so they settle side by side rather than one orchestrator
// round trip at a time, but the next batch waits for the last to commit.
// Every seed debits the one reserve Wallet, so seeds preparing together race
// on its stream, and every loser re-plans after a backoff (go/docs/adr/0003).
// batch bounds that race however many entities there are, or however many
// partitions the orchestrator runs concurrently, so seeding, which is setup
// rather than the load being measured, does not spend the run on retries.
// It must be at least 1.
func seedAll(
	ctx context.Context, transactions transactionpb.TransactionService,
	reserve entity, entities []entity, amount uint64, currency string, batch int, wait settleWait,
) error {
	for start := 0; start < len(entities); start += batch {
		end := min(start+batch, len(entities))
		if err := seedBatch(ctx, transactions, reserve, entities[start:end], amount, currency, wait); err != nil {
			return err
		}
	}
	return nil
}

// seedBatch starts a seed for every one of entities, then waits for each to
// commit.
func seedBatch(
	ctx context.Context, transactions transactionpb.TransactionService,
	reserve entity, entities []entity, amount uint64, currency string, wait settleWait,
) error {
	seeds := make([]txResult, len(entities))
	for i, target := range entities {
		transactionID, err := startSeed(ctx, transactions, reserve, target, amount, currency)
		if err != nil {
			return err
		}
		seeds[i] = txResult{transactionID: transactionID}
	}

	observe := func(ctx context.Context, r *txResult) { observeTransaction(ctx, transactions, r) }
	for i, target := range entities {
		seed := &seeds[i]
		awaitResult(ctx, seed, wait, observe)
		switch {
		case seed.err != nil:
			return fmt.Errorf("seed %s: %w", target.name, seed.err)
		case seed.moved:
			continue
		case seed.open:
			return fmt.Errorf("seed %s: still %s after waiting; nothing is folding it — check go/cmd/orchestrator is "+
				"running with a CDC connector to consume from (`make cdc-up && make orchestrator-up`, then "+
				"`make orchestrator-logs`)", target.name, seed.final)
		default:
			return fmt.Errorf("seed %s: ended %s rather than completing: %s", target.name, seed.final, seed.reason)
		}
	}
	return nil
}

// startSeed asks for target's seed and reports the Transaction it started. A
// seed has no staging and no business decision to wait on, so once the
// orchestrator folds it, it commits on its own.
func startSeed(
	ctx context.Context, transactions transactionpb.TransactionService, reserve, target entity, amount uint64, currency string,
) (string, error) {
	transactionID, transferID := uuid.NewV7().String(), uuid.NewV7().String()
	resp, err := transactions.StartInitializingTransaction(ctx, &transactionpb.StartInitializingTransactionRequest{
		Id: transactionID, FactoryName: "simulate_seed", FactoryVersion: "v1",
		Transfers: map[string]*transactionpb.Transfer{
			transferID: {
				Id: transferID, FromWalletId: reserve.walletID, ToWalletId: target.walletID,
				Amount:     &sharedpb.Money{MinorUnits: amount, Currency: currency},
				MintSource: true,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("seed %s: %w", target.name, err)
	}
	if rejected := resp.GetTransactionRejected(); rejected != nil {
		return "", fmt.Errorf("seed %s: rejected: %s", target.name, rejected.GetReason())
	}
	return transactionID, nil
}

// driveFunc drives one simulated transfer between from and to end to end,
// however -mode chooses to shape it — a bare Transfer, or one wrapped in a
// single-child Transaction. runLoad knows nothing about that shape; it only
// calls drive and collects the txResult back.
type driveFunc func(ctx context.Context, from, to entity, amount uint64, currency string, planned outcome) txResult

// fromTransactionState translates a Transaction's proto state into this
// package's own moved/open vocabulary. COMPLETED is the only state that moved
// money. Every state the saga has yet to finish with is open — INITIALIZED and
// ROLLBACK_STARTED as much as STARTED, since each is waiting on
// go/cmd/orchestrator rather than on anything this tool will decide.
func fromTransactionState(state transactionpb.TransactionState) (final string, moved, open bool) {
	switch state {
	case transactionpb.TransactionState_TRANSACTION_STATE_INITIALIZED,
		transactionpb.TransactionState_TRANSACTION_STATE_STARTED,
		transactionpb.TransactionState_TRANSACTION_STATE_ROLLBACK_STARTED:
		open = true
	}
	return state.String(), state == transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED, open
}

// fromTransferOutcome is fromTransactionState's counterpart for a bare
// Transfer, labelled after transfer.OutcomeKind's own names so the report and
// the Transfer aggregate never disagree on what a state is called. Anything
// short of a terminal is open: in flight is the orchestrator's to move, and
// staged or pending is this tool's to settle.
func fromTransferOutcome(o transfer.OutcomeKind) (final string, moved, open bool) {
	switch o {
	case transfer.OutcomeNotFound, transfer.OutcomeInFlight, transfer.OutcomeStaged, transfer.OutcomePending:
		open = true
	}
	return "TRANSFER_" + strings.ToUpper(o.String()), o == transfer.OutcomeCommitted, open
}

// observeTransaction records where r's Transaction stands. Its reason, when
// it has one, replaces whatever r last held: the Transaction's own account of
// how it ended is the one worth reporting.
func observeTransaction(ctx context.Context, transactions transactionpb.TransactionService, r *txResult) {
	resp, err := transactions.GetTransactionState(ctx, &transactionpb.GetTransactionStateRequest{Id: r.transactionID})
	if err != nil {
		r.err = err
		return
	}
	r.final, r.moved, r.open = fromTransactionState(resp.GetState())
	if reason := resp.GetReason(); reason != "" {
		r.reason = reason
	}
}

// transactionDriver runs -mode=transaction: every simulated Transfer wrapped
// in a single-child Transaction, the production ACH shape.
type transactionDriver struct {
	transactions transactionpb.TransactionService
	transfers    transferpb.TransferService
	leg          legReader
	wait         settleWait
}

// drive runs one simulated transaction end to end: a staged Transfer between
// from and to, wrapped in a Transaction, steered toward planned once it has
// staged — the window a real staged Transfer waits in for its provider — and
// reported in whatever state Go actually left it. That may differ from
// planned when the Transfer fails organically (e.g. insufficient funds)
// before it ever stages.
func (d transactionDriver) drive(ctx context.Context, from, to entity, amount uint64, currency string, planned outcome) txResult {
	start := time.Now()
	transactionID, transferID := uuid.NewV7().String(), uuid.NewV7().String()
	r := txResult{
		transactionID: transactionID, transferID: transferID, fromWallet: from.walletID, toWallet: to.walletID,
		amountMinor: int64(amount), planned: planned,
	}

	resp, err := d.transactions.StartInitializingTransaction(ctx, &transactionpb.StartInitializingTransactionRequest{
		Id: transactionID, FactoryName: "simulate", FactoryVersion: "v1",
		Transfers: map[string]*transactionpb.Transfer{
			transferID: {
				Id: transferID, FromWalletId: from.walletID, ToWalletId: to.walletID,
				Amount: &sharedpb.Money{MinorUnits: amount, Currency: currency},
				Stage:  true,
			},
		},
	})
	switch {
	case err != nil:
		r.err = err
	case resp.GetTransactionRejected() != nil:
		// Refused at the door, funding pre-flight most likely: terminal, and
		// there is no leg to wait for.
		r.final, r.moved, r.open = fromTransactionState(transactionpb.TransactionState_TRANSACTION_STATE_REJECTED)
		r.reason = resp.GetTransactionRejected().GetReason()
	default:
		awaitResult(ctx, &r, d.wait, d.advance)
	}
	r.latency = time.Since(start)
	return r
}

// advance settles r's leg the first time a look finds it staged, then records
// the Transaction's own state. Until then the leg is the orchestrator's to
// move: not yet requested, or still preparing. A leg that resolves without
// ever staging — an organic failure — leaves nothing for the tool to decide;
// the Transaction rolls itself back, and all that is left is to see it land.
func (d transactionDriver) advance(ctx context.Context, r *txResult) {
	if !r.settled {
		leg, err := d.leg(ctx, r.transferID)
		if err != nil {
			r.err = err
			return
		}
		switch leg {
		case transfer.OutcomeNotFound, transfer.OutcomeInFlight:
			// Not staged yet, and not this tool's to hurry.
		case transfer.OutcomeStaged, transfer.OutcomePending:
			reason, err := d.settle(ctx, r, leg)
			if err != nil {
				r.err = err
				return
			}
			r.settled, r.reason = true, reason
		default:
			r.settled = true
		}
	}
	observeTransaction(ctx, d.transactions, r)
}

// settle steers r's staged leg toward planned: the provider reporting the
// entry settled, or returned. Rolling back is the Transaction's to do, so it
// is asked of the Transaction — legal now, since a staged leg means the
// Transaction has started. Completing is the leg's own confirmation and
// posting. Reports a refusal's reason, if one came back.
func (d transactionDriver) settle(ctx context.Context, r *txResult, leg transfer.OutcomeKind) (string, error) {
	if r.planned == outcomeRollback {
		_, err := d.transactions.StartTransactionRollback(ctx, &transactionpb.StartTransactionRollbackRequest{
			Id: r.transactionID, Reason: simulatedRollover,
		})
		return "", err
	}
	return confirmAndPost(ctx, d.transfers, r.transferID, leg)
}

// transferDriver runs -mode=transfer: bare Transfers with no owning
// Transaction, to isolate TransferService's own throughput from
// Transaction's DAG-dispatch overhead. It mirrors transactionDriver except for
// the missing wrapper.
type transferDriver struct {
	transfers transferpb.TransferService
	leg       legReader
	wait      settleWait
}

func (d transferDriver) drive(ctx context.Context, from, to entity, amount uint64, currency string, planned outcome) txResult {
	start := time.Now()
	r := txResult{
		transferID: uuid.NewV7().String(), fromWallet: from.walletID, toWallet: to.walletID,
		amountMinor: int64(amount), planned: planned,
	}

	resp, err := d.transfers.RequestTransfer(ctx, &transferpb.RequestTransferRequest{
		Id: r.transferID, FromWalletId: from.walletID, ToWalletId: to.walletID,
		Amount: &sharedpb.Money{MinorUnits: amount, Currency: currency}, Stage: true,
	})
	switch {
	case err != nil:
		r.err = err
	case resp.GetTransferRequestRejected() != nil:
		// An organic failure (e.g. insufficient funds) decided at request
		// time: terminal, and it never moved anything.
		r.final, r.moved, r.open = fromTransferOutcome(transfer.OutcomeRejected)
		r.reason = resp.GetTransferRequestRejected().GetReason()
	default:
		awaitResult(ctx, &r, d.wait, d.advance)
	}
	r.latency = time.Since(start)
	return r
}

// advance settles r the first time a look finds it staged, then records where
// it landed. The settlement RPCs are each one claimed transition answered in
// full, so a bare Transfer is terminal the moment its settlement returns —
// there is nothing downstream of it to wait for.
func (d transferDriver) advance(ctx context.Context, r *txResult) {
	leg, err := d.leg(ctx, r.transferID)
	if err != nil {
		r.err = err
		return
	}
	if !r.settled && (leg == transfer.OutcomeStaged || leg == transfer.OutcomePending) {
		reason, err := d.settle(ctx, r, leg)
		if err != nil {
			r.err = err
			return
		}
		r.settled, r.reason = true, reason
		if leg, err = d.leg(ctx, r.transferID); err != nil {
			r.err = err
			return
		}
	}
	r.final, r.moved, r.open = fromTransferOutcome(leg)
}

// settle steers r's staged Transfer toward planned: cancelled directly (the
// provider returning the entry), or confirmed and posted.
func (d transferDriver) settle(ctx context.Context, r *txResult, leg transfer.OutcomeKind) (string, error) {
	if r.planned == outcomeRollback {
		resp, err := d.transfers.CancelStagedTransfer(ctx, &transferpb.CancelStagedTransferRequest{
			Id: r.transferID, Reason: simulatedRollover,
		})
		if err != nil {
			return "", err
		}
		if rejected := resp.GetCancelStagedTransferRejected(); rejected != nil {
			return rejected.GetReason(), nil
		}
		return simulatedRollover, nil
	}
	return confirmAndPost(ctx, d.transfers, r.transferID, leg)
}

// confirmAndPost completes a staged or pending leg: confirmed (the provider
// accepted the entry), then posted (the funds settled). A pending leg was
// already confirmed and only needs posting. Reports a refusal's reason, if
// either step refused.
func confirmAndPost(ctx context.Context, transfers transferpb.TransferService, transferID string, leg transfer.OutcomeKind) (string, error) {
	if leg == transfer.OutcomeStaged {
		resp, err := transfers.ConfirmStagedTransfer(ctx, &transferpb.ConfirmStagedTransferRequest{Id: transferID})
		if err != nil {
			return "", err
		}
		if rejected := resp.GetConfirmStagedTransferRejected(); rejected != nil {
			return rejected.GetReason(), nil
		}
	}

	resp, err := transfers.PostPendingTransfer(ctx, &transferpb.PostPendingTransferRequest{Id: transferID})
	if err != nil {
		return "", err
	}
	if rejected := resp.GetPostPendingTransferRejected(); rejected != nil {
		return rejected.GetReason(), nil
	}
	return "", nil
}

// runLoad drives n simulated transfers across entities with concurrency
// bounded workers, however drive chooses to shape each one. Each worker
// seeds its own *rand.Rand from (seed, i), so the whole run is reproducible
// for a fixed seed regardless of scheduling, without a shared RNG's lock
// contention under load.
func runLoad(
	ctx context.Context, entities []entity, cfg runConfig, n, concurrency int, seed uint64, drive driveFunc,
) ([]txResult, time.Duration) {
	results := make([]txResult, n)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			r := rand.New(rand.NewPCG(seed, uint64(i)))
			from, to := pickPair(r, len(entities))
			amount := cfg.minAmount
			if cfg.maxAmount > cfg.minAmount {
				amount += uint64(r.IntN(int(cfg.maxAmount-cfg.minAmount) + 1))
			}
			planned := pickOutcome(r, cfg.rollbackRate)

			results[i] = drive(ctx, entities[from], entities[to], amount, cfg.currency, planned)
		}(i)
	}
	wg.Wait()
	return results, time.Since(start)
}
