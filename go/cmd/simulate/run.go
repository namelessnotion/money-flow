package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
	"uuid"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
)

// runConfig is everything one simulated transaction needs to pick its
// amount and outcome, independent of which worker runs it.
type runConfig struct {
	currency     string
	minAmount    uint64
	maxAmount    uint64
	rollbackRate float64
}

// seedEntity mints amount fresh into target's wallet from reserve, via a
// single mint_source Transfer — the simulated equivalent of an ACH
// deposit's shadow leg recognizing new cash in. stage=false, auto_process=
// true commits synchronously within the one StartInitializingTransaction
// call, so the seed is in place before this returns.
func seedEntity(ctx context.Context, transactions transactionpb.TransactionService, reserve, target entity, amount uint64, currency string) error {
	transactionID, transferID := uuid.NewV7().String(), uuid.NewV7().String()
	resp, err := transactions.StartInitializingTransaction(ctx, &transactionpb.StartInitializingTransactionRequest{
		Id: transactionID, FactoryName: "simulate_seed", FactoryVersion: "v1",
		Transfers: map[string]*transactionpb.Transfer{
			transferID: {
				Id: transferID, FromWalletId: reserve.walletID, ToWalletId: target.walletID,
				Amount:      &sharedpb.Money{MinorUnits: amount, Currency: currency},
				AutoProcess: true, MintSource: true,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("seed %s: %w", target.name, err)
	}
	if rejected := resp.GetTransactionRejected(); rejected != nil {
		return fmt.Errorf("seed %s: rejected: %s", target.name, rejected.GetReason())
	}
	return nil
}

// driveFunc drives one simulated transfer between from and to end to end,
// however -mode chooses to shape it — a bare Transfer, or one wrapped in a
// single-child Transaction. runLoad knows nothing about that shape; it only
// calls drive and collects the txResult back.
type driveFunc func(ctx context.Context, from, to entity, amount uint64, currency string, planned outcome) txResult

// fromTransactionState translates a Transaction's proto state into this
// package's own moved/open vocabulary: COMPLETED is the only state that
// actually moved money, and STARTED is the only one still open (waiting on
// this tool's own next call, or on go/cmd/orchestrator).
func fromTransactionState(state transactionpb.TransactionState) (final string, moved, open bool) {
	return state.String(),
		state == transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED,
		state == transactionpb.TransactionState_TRANSACTION_STATE_STARTED
}

// driveOne runs one simulated transaction end to end: stage a Transfer
// between from and to, wrapped in a single-child Transaction (the
// production ACH shape), then steer it toward planned, and report whatever
// state Go actually settled it in — which may differ from planned when the
// Transfer fails organically (e.g. insufficient funds) before ever reaching
// Staged.
func driveOne(
	ctx context.Context,
	transactions transactionpb.TransactionService, transfers transferpb.TransferService,
	from, to entity, amount uint64, currency string, planned outcome,
) txResult {
	start := time.Now()
	transactionID, transferID := uuid.NewV7().String(), uuid.NewV7().String()
	result := txResult{
		transactionID: transactionID, transferID: transferID, fromWallet: from.walletID, toWallet: to.walletID,
		amountMinor: int64(amount), planned: planned,
	}

	_, err := transactions.StartInitializingTransaction(ctx, &transactionpb.StartInitializingTransactionRequest{
		Id: transactionID, FactoryName: "simulate", FactoryVersion: "v1",
		Transfers: map[string]*transactionpb.Transfer{
			transferID: {
				Id: transferID, FromWalletId: from.walletID, ToWalletId: to.walletID,
				Amount:      &sharedpb.Money{MinorUnits: amount, Currency: currency},
				AutoProcess: true, Stage: true,
			},
		},
	})
	if err != nil {
		result.err = err
		result.latency = time.Since(start)
		return result
	}

	state, reason, err := resumeTransaction(ctx, transactions, transactionID)
	if err == nil && state == transactionpb.TransactionState_TRANSACTION_STATE_STARTED {
		// Still Started means the Transfer leg staged cleanly and is
		// waiting on us, exactly the window a real staged Transfer waits in
		// for the provider — steer it toward planned. Any other state here
		// is already terminal (an organic failure rolled it back on its
		// own before we got a say).
		state, reason, err = settle(ctx, transactions, transfers, transactionID, transferID, planned)
	}

	result.final, result.moved, result.open = fromTransactionState(state)
	result.reason, result.err = reason, err
	result.latency = time.Since(start)
	return result
}

func resumeTransaction(ctx context.Context, transactions transactionpb.TransactionService, id string) (transactionpb.TransactionState, string, error) {
	resp, err := transactions.ResumeTransaction(ctx, &transactionpb.ResumeTransactionRequest{Id: id})
	if err != nil {
		return transactionpb.TransactionState_TRANSACTION_STATE_UNSPECIFIED, "", err
	}
	return resp.GetState(), resp.GetReason(), nil
}

// settle steers a Transaction whose sole Transfer leg is Staged toward
// planned: complete confirms and posts the staged leg (the provider
// reporting the entry settled) then resumes the Transaction to notice;
// rollback asks the Transaction itself to cancel it (the provider returning
// the entry instead).
func settle(
	ctx context.Context,
	transactions transactionpb.TransactionService, transfers transferpb.TransferService,
	transactionID, transferID string, planned outcome,
) (transactionpb.TransactionState, string, error) {
	if planned == outcomeRollback {
		resp, err := transactions.StartTransactionRollback(ctx, &transactionpb.StartTransactionRollbackRequest{
			Id: transactionID, Reason: "simulated rollover",
		})
		if err != nil {
			return transactionpb.TransactionState_TRANSACTION_STATE_UNSPECIFIED, "", err
		}
		return resp.GetState(), "simulated rollover", nil
	}

	confirmResp, err := transfers.ConfirmStagedTransfer(ctx, &transferpb.ConfirmStagedTransferRequest{Id: transferID})
	if err != nil {
		return transactionpb.TransactionState_TRANSACTION_STATE_UNSPECIFIED, "", err
	}
	if rejected := confirmResp.GetConfirmStagedTransferRejected(); rejected != nil {
		// The Transfer's own saga never reached Staged — most likely it lost
		// a concurrency race while preparing its source Tokens under load
		// (transfer.Server bounds those retries) and is parked mid-flight.
		// Nothing this tool calls drives that saga forward again; only
		// go/cmd/orchestrator's event-triggered Resume does. Report it as
		// still Started with the rejection's reason rather than as a
		// transport error: that is genuinely the Transaction's state right
		// now, and it is worth surfacing rather than hiding behind err.
		return transactionpb.TransactionState_TRANSACTION_STATE_STARTED, rejected.GetReason(), nil
	}

	postResp, err := transfers.PostPendingTransfer(ctx, &transferpb.PostPendingTransferRequest{Id: transferID})
	if err != nil {
		return transactionpb.TransactionState_TRANSACTION_STATE_UNSPECIFIED, "", err
	}
	if rejected := postResp.GetPostPendingTransferRejected(); rejected != nil {
		return transactionpb.TransactionState_TRANSACTION_STATE_STARTED, rejected.GetReason(), nil
	}

	return resumeTransaction(ctx, transactions, transactionID)
}

// driveOneTransfer runs one simulated Transfer end to end with no owning
// Transaction at all — RequestTransfer directly, then steer it toward
// planned — to isolate TransferService's own throughput from Transaction's
// DAG-dispatch overhead. It is the -mode=transfer counterpart to driveOne,
// and mirrors its shape exactly except for the missing Transaction wrapper:
// stage=true parks a cleanly-accepted Transfer at Staged within the same
// RequestTransfer call, the same way driveOne's child Transfer does, and
// settleTransfer steers it from there exactly as settle does.
func driveOneTransfer(
	ctx context.Context, transfers transferpb.TransferService,
	from, to entity, amount uint64, currency string, planned outcome,
) txResult {
	start := time.Now()
	transferID := uuid.NewV7().String()
	result := txResult{
		transferID: transferID, fromWallet: from.walletID, toWallet: to.walletID,
		amountMinor: int64(amount), planned: planned,
	}

	resp, err := transfers.RequestTransfer(ctx, &transferpb.RequestTransferRequest{
		Id: transferID, FromWalletId: from.walletID, ToWalletId: to.walletID,
		Amount: &sharedpb.Money{MinorUnits: amount, Currency: currency}, Stage: true,
	})
	if err != nil {
		result.err = err
		result.latency = time.Since(start)
		return result
	}
	if rejected := resp.GetTransferRequestRejected(); rejected != nil {
		// An organic failure (e.g. insufficient funds) decided at request
		// time, before ever reaching Staged — terminal, and never moved
		// anything, the same as driveOne's own request-time rejections.
		result.final, result.reason = "TRANSFER_REQUEST_REJECTED", rejected.GetReason()
		result.latency = time.Since(start)
		return result
	}

	result.final, result.moved, result.open, result.reason, result.err = settleTransfer(ctx, transfers, transferID, planned)
	result.latency = time.Since(start)
	return result
}

// settleTransfer steers a bare, Staged Transfer toward planned: complete
// confirms and posts it (the provider reporting the entry settled);
// rollback cancels it directly (the provider returning the entry instead).
// It is settle's -mode=transfer counterpart — same shape, minus the
// Transaction-level rollback/resume indirection a bare Transfer has no use
// for — and reports the same "still open" signal settle does when the
// Transfer never reached Staged in the first place.
func settleTransfer(
	ctx context.Context, transfers transferpb.TransferService, transferID string, planned outcome,
) (final string, moved, open bool, reason string, err error) {
	if planned == outcomeRollback {
		resp, err := transfers.CancelStagedTransfer(ctx, &transferpb.CancelStagedTransferRequest{
			Id: transferID, Reason: "simulated rollover",
		})
		if err != nil {
			return "", false, false, "", err
		}
		if rejected := resp.GetCancelStagedTransferRejected(); rejected != nil {
			// Not (yet) Staged — most likely lost a concurrency race while
			// preparing, the same condition settle's own
			// ConfirmStagedTransferRejected case reports. Still open: only
			// go/cmd/orchestrator's event-triggered Resume (or a later call
			// touching this id) drives it forward from here.
			return "TRANSFER_ACCEPTED", false, true, rejected.GetReason(), nil
		}
		return "TRANSFER_CANCELLED", false, false, "simulated rollover", nil
	}

	confirmResp, err := transfers.ConfirmStagedTransfer(ctx, &transferpb.ConfirmStagedTransferRequest{Id: transferID})
	if err != nil {
		return "", false, false, "", err
	}
	if rejected := confirmResp.GetConfirmStagedTransferRejected(); rejected != nil {
		return "TRANSFER_ACCEPTED", false, true, rejected.GetReason(), nil
	}

	postResp, err := transfers.PostPendingTransfer(ctx, &transferpb.PostPendingTransferRequest{Id: transferID})
	if err != nil {
		return "", false, false, "", err
	}
	if rejected := postResp.GetPostPendingTransferRejected(); rejected != nil {
		return "TRANSFER_PENDING", false, true, rejected.GetReason(), nil
	}
	return "TRANSFER_COMMITTED", true, false, "", nil
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

// retryFunc re-attempts settling one stuck result in place — mutating its
// final/moved/open/reason/err — however -mode chooses to shape a retry.
type retryFunc func(ctx context.Context, r *txResult)

// retryStuck gives every stuck result another chance to settle, waiting
// delay before each attempt to let go/cmd/orchestrator's event-triggered
// Resume have a chance to advance any Transfer parked at Accepted (see
// settle's/settleTransfer's ConfirmStagedTransferRejected case) to Staged —
// the furthest an external, business-decision-free retry can take it.
// Nothing else drives that saga forward: this tool's own first settling
// attempt is the only other caller, and it already ran once inside drive.
// Mutates results in place and returns however many are still stuck once
// attempts run out.
func retryStuck(ctx context.Context, results []txResult, attempts int, delay time.Duration, retry retryFunc) int {
	for attempt := 0; attempt < attempts; attempt++ {
		idx := stuckIndices(results)
		if len(idx) == 0 {
			return 0
		}
		time.Sleep(delay)
		for _, i := range idx {
			start := time.Now()
			retry(ctx, &results[i])
			results[i].latency += time.Since(start)
		}
	}
	return len(stuckIndices(results))
}

func stuckIndices(results []txResult) []int {
	var idx []int
	for i, r := range results {
		if r.stuck() {
			idx = append(idx, i)
		}
	}
	return idx
}
