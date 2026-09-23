package main

import (
	"context"
	"slices"
	"testing"

	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

const (
	txInitialized    = transactionpb.TransactionState_TRANSACTION_STATE_INITIALIZED
	txStarted        = transactionpb.TransactionState_TRANSACTION_STATE_STARTED
	txRollbackBegun  = transactionpb.TransactionState_TRANSACTION_STATE_ROLLBACK_STARTED
	txCompleted      = transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED
	txRolledBack     = transactionpb.TransactionState_TRANSACTION_STATE_ROLLED_BACK
	txRollbackFailed = transactionpb.TransactionState_TRANSACTION_STATE_ROLLBACK_FAILED
	txRejected       = transactionpb.TransactionState_TRANSACTION_STATE_REJECTED
)

// A wait that never actually sleeps, long enough for every script below.
var testWait = settleWait{attempts: 10}

func newTransactionDriver(transactions *scriptedTransactions, leg *scriptedLeg) (transactionDriver, *recordingTransfers) {
	transactions.leg = leg
	transfers := &recordingTransfers{leg: leg}
	return transactionDriver{transactions: transactions, transfers: transfers, leg: leg.read, wait: testWait}, transfers
}

func driveTransaction(d transactionDriver, planned outcome) txResult {
	return d.drive(context.Background(), entity{walletID: "from"}, entity{walletID: "to"}, 500, "USD", planned)
}

// The bug this shape exists to fix: a Transaction is still Initialized when the
// tool first looks, because accepting it is all StartInitializingTransaction
// does. That is a wait, not an answer — and the leg is settled only once it
// has actually staged, never on the strength of the Transaction being Started.
func TestTransactionDriver_WaitsThroughInitializedAndSettlesOnlyOnceStaged(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeNotFound, transfer.OutcomeInFlight, transfer.OutcomeStaged)
	transactions := newScriptedTransactions(txInitialized, txStarted, txStarted, txCompleted)
	d, transfers := newTransactionDriver(transactions, leg)

	r := driveTransaction(d, outcomeComplete)

	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if want := []string{"confirm at staged", "post at staged"}; !slices.Equal(transfers.calls, want) {
		t.Errorf("settlement calls = %q, want %q — confirmed exactly once, and only after the leg was seen staged", transfers.calls, want)
	}
	if r.final != "TRANSACTION_STATE_COMPLETED" || !r.moved || r.open {
		t.Errorf("result = %s moved=%v open=%v, want COMPLETED, moved, closed", r.final, r.moved, r.open)
	}
	if got := leg.totalLooks(); got != 3 {
		t.Errorf("leg looked at %d times, want 3 — once settled, only the Transaction's outcome is left to see", got)
	}
}

// A planned rollback is the Transaction's own call to make, and it is only
// legal once the Transaction has started. Asking only after the leg has staged
// means asking of a Transaction that has — and waiting through
// rollback_started is waiting for the orchestrator to cancel the leg.
func TestTransactionDriver_RollsBackOnlyOnceStagedAndWaitsForTheRollbackToLand(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeInFlight, transfer.OutcomeStaged)
	transactions := newScriptedTransactions(txStarted, txStarted, txRollbackBegun, txRolledBack)
	d, transfers := newTransactionDriver(transactions, leg)

	r := driveTransaction(d, outcomeRollback)

	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if want := []string{"rollback at staged"}; !slices.Equal(transactions.rollbacks, want) {
		t.Errorf("rollbacks = %q, want %q", transactions.rollbacks, want)
	}
	if len(transfers.calls) != 0 {
		t.Errorf("transfer calls = %q, want none — rolling back is the Transaction's job, not the leg's", transfers.calls)
	}
	if r.final != "TRANSACTION_STATE_ROLLED_BACK" || r.moved || r.open {
		t.Errorf("result = %s moved=%v open=%v, want ROLLED_BACK, not moved, closed", r.final, r.moved, r.open)
	}
}

// A leg that fails on its own before it stages — insufficient funds, say — is
// not the tool's to steer. The Transaction rolls itself back; the tool waits
// to see that and calls nothing.
func TestTransactionDriver_LeavesALegThatFailedOnItsOwnAlone(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeInFlight, transfer.OutcomeFailed)
	transactions := newScriptedTransactions(txStarted, txRollbackBegun, txRolledBack)
	d, transfers := newTransactionDriver(transactions, leg)

	r := driveTransaction(d, outcomeComplete)

	if len(transfers.calls) != 0 || len(transactions.rollbacks) != 0 {
		t.Errorf("calls = %q rollbacks = %q, want none", transfers.calls, transactions.rollbacks)
	}
	if r.final != "TRANSACTION_STATE_ROLLED_BACK" || r.moved || r.open {
		t.Errorf("result = %s moved=%v open=%v, want ROLLED_BACK, not moved, closed", r.final, r.moved, r.open)
	}
}

// A Transaction refused at the door has no leg to wait for, ever.
func TestTransactionDriver_ReportsARejectionWithoutWaiting(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeNotFound)
	transactions := newScriptedTransactions(txInitialized)
	transactions.rejectAs = "insufficient funds"
	d, _ := newTransactionDriver(transactions, leg)

	r := driveTransaction(d, outcomeComplete)

	if r.final != "TRANSACTION_STATE_REJECTED" || r.reason != "insufficient funds" || r.open || r.moved {
		t.Errorf("result = %s (%q) moved=%v open=%v, want REJECTED (insufficient funds), closed", r.final, r.reason, r.moved, r.open)
	}
	if got := leg.totalLooks(); got != 0 {
		t.Errorf("leg looked at %d times, want 0", got)
	}
}

// Out of patience, the result says where things stood — still open, and
// settled nothing it had not seen staged.
func TestTransactionDriver_GivesUpStillOpenAndUnsettled(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeInFlight)
	transactions := newScriptedTransactions(txStarted)
	d, transfers := newTransactionDriver(transactions, leg)
	d.wait = settleWait{attempts: 3}

	r := driveTransaction(d, outcomeComplete)

	if !r.open || r.final != "TRANSACTION_STATE_STARTED" {
		t.Errorf("result = %s open=%v, want STARTED and still open", r.final, r.open)
	}
	if len(transfers.calls) != 0 {
		t.Errorf("transfer calls = %q, want none", transfers.calls)
	}
	if got := leg.totalLooks(); got != 4 {
		t.Errorf("leg looked at %d times, want 4 — one look, then -settle-wait-attempts more", got)
	}
}

// A result left open by the load gets picked up again by the catch-up pass,
// and carries on from where it was rather than starting over: a leg already
// settled is never settled twice.
func TestAwaitStuck_ResumesWhereEachResultLeftOff(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeStaged)
	transactions := newScriptedTransactions(txStarted, txStarted, txCompleted)
	d, transfers := newTransactionDriver(transactions, leg)
	d.wait = settleWait{attempts: 0}

	r := driveTransaction(d, outcomeComplete)
	if !r.open {
		t.Fatalf("result = %s, want it still open after a single look", r.final)
	}

	results := []txResult{r, {final: "TRANSACTION_STATE_ROLLED_BACK"}}
	if remaining := awaitStuck(context.Background(), results, testWait, d.advance); remaining != 0 {
		t.Errorf("awaitStuck remaining = %d, want 0", remaining)
	}
	if results[0].final != "TRANSACTION_STATE_COMPLETED" {
		t.Errorf("results[0].final = %s, want COMPLETED", results[0].final)
	}
	if want := []string{"confirm at staged", "post at staged"}; !slices.Equal(transfers.calls, want) {
		t.Errorf("settlement calls = %q, want %q — settled once, during the load", transfers.calls, want)
	}
}

func TestStuckIndices(t *testing.T) {
	t.Parallel()
	results := []txResult{
		{final: "TRANSACTION_STATE_COMPLETED", moved: true},
		{final: "TRANSACTION_STATE_STARTED", open: true},
		{final: "TRANSACTION_STATE_ROLLED_BACK"},
		{final: "TRANSACTION_STATE_INITIALIZED", open: true},
	}
	if got, want := stuckIndices(results), []int{1, 3}; !slices.Equal(got, want) {
		t.Errorf("stuckIndices = %v, want %v", got, want)
	}
}

// Open is every state the orchestrator has yet to finish with. Initialized and
// rollback_started are as open as started: each is a Transaction mid-saga.
func TestFromTransactionState(t *testing.T) {
	t.Parallel()
	for state, want := range map[transactionpb.TransactionState]struct{ moved, open bool }{
		txInitialized:    {open: true},
		txStarted:        {open: true},
		txRollbackBegun:  {open: true},
		txCompleted:      {moved: true},
		txRolledBack:     {},
		txRollbackFailed: {},
		txRejected:       {},
	} {
		final, moved, open := fromTransactionState(state)
		if final != state.String() || moved != want.moved || open != want.open {
			t.Errorf("fromTransactionState(%s) = (%s, moved=%v, open=%v), want moved=%v open=%v",
				state, final, moved, open, want.moved, want.open)
		}
	}
}
