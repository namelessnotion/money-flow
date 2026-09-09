package saga

import (
	"context"
	"sync"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// achWorld provisions the four Wallets the ACH deposit worked example needs.
func achWorld(t *testing.T) (*world, string, string, string, string) {
	t.Helper()
	w := newWorld(t, eventstore.NewMemoryStore(), ledger.NewFakeClient())
	bankAccount := testutil.ID("bank-account")
	cash := testutil.ID("cash")
	bankControl := testutil.ID("bank-control")
	uncleared := testutil.ID("uncleared")
	w.openWallet(bankAccount, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	w.openWallet(cash, sharedpb.Allows_ALLOWS_NONE)
	w.openWallet(bankControl, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	w.openWallet(uncleared, sharedpb.Allows_ALLOWS_NONE)
	return w, bankAccount, cash, bankControl, uncleared
}

// achDeposit is the ACH deposit shape: a staged real leg that settles over
// days, and an unstaged shadow leg that may only run once the real one has
// completed. The Transaction is initialized and nothing else — no synchronous
// dispatch has happened, and only triggers will move it.
func achDeposit(t *testing.T) (w *world, txnID, realID, shadowID string) {
	t.Helper()
	w, bankAccount, cash, bankControl, uncleared := achWorld(t)
	txnID = testutil.ID("txn-deposit")
	realID = testutil.ID("real")
	shadowID = testutil.ID("shadow")
	w.initialize(txnID,
		map[string]*transactionpb.Transfer{
			realID: {
				Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash,
				AutoProcess: true, Stage: true, MintSource: true,
			},
			shadowID: {
				Id: shadowID, Amount: usd(10000), FromWalletId: bankControl, ToWalletId: uncleared,
				AutoProcess: true, Stage: false, MintSource: true,
			},
		},
		map[string]*transactionpb.TransferIdList{shadowID: {TransferId: []string{realID}}},
	)
	return w, txnID, realID, shadowID
}

// The headline claim: with nothing dispatching the saga in process, the two
// topics between them carry a Transaction with children all the way to
// completed.
//
// The staged child is what makes both topics load-bearing. It settles through
// confirmations the outside world owns, none of which touch the Transaction,
// so the Transaction learns its child committed only when the transfer topic
// tells it to go and look.
func TestOrchestrator_DrivesATransactionToCompletedFromTriggersAlone(t *testing.T) {
	t.Parallel()
	w, txnID, realID, shadowID := achDeposit(t)

	// The transaction topic starts it: initialized -> started, real dispatched.
	w.mustDeliver(transaction.AggregateType, txnID)
	if got := w.outcome(realID); got != transfer.OutcomeStaged {
		t.Fatalf("real Outcome() = %v, want staged", got)
	}
	if got := w.outcome(shadowID); got != transfer.OutcomeNotFound {
		t.Fatalf("shadow Outcome() = %v, want not_found: it depends on real completing", got)
	}

	// ...days pass, ACH settles. The Transaction is told nothing.
	w.settle(realID)
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_STARTED {
		t.Fatalf("state = %v, want still STARTED before the trigger arrives", got)
	}

	// The transfer topic finishes it: the committed child is reconciled, the
	// shadow leg becomes ready, and the Transaction completes.
	w.mustDeliver(transfer.AggregateType, realID)
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Fatalf("state = %v, want COMPLETED", got)
	}
	if got := w.outcome(shadowID); got != transfer.OutcomeCommitted {
		t.Errorf("shadow Outcome() = %v, want committed", got)
	}
}

// Merging this before the cutover is only safe if the orchestrator does
// nothing at all where the synchronous saga already did the work. Here the
// whole Transaction runs the old way, and then every message it would have
// published is delivered.
func TestOrchestrator_IsANoOpWhenTheSynchronousPathAlreadyFinished(t *testing.T) {
	t.Parallel()
	w, bankAccount, cash, bankControl, uncleared := achWorld(t)
	txnID := testutil.ID("txn-sync")
	realID := testutil.ID("real")
	shadowID := testutil.ID("shadow")

	// The ordinary RPC, saga and all: no staging anywhere, so it runs to
	// completion inside the call.
	if _, err := w.transactions.StartInitializingTransaction(context.Background(), &transactionpb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*transactionpb.Transfer{
			realID:   {Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash, AutoProcess: true, MintSource: true},
			shadowID: {Id: shadowID, Amount: usd(10000), FromWalletId: bankControl, ToWalletId: uncleared, AutoProcess: true, MintSource: true},
		},
		TransferDependency: map[string]*transactionpb.TransferIdList{shadowID: {TransferId: []string{realID}}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Fatalf("state = %v, want COMPLETED from the synchronous path", got)
	}

	before, _ := w.store.counts()
	for _, id := range []string{realID, shadowID} {
		w.mustDeliver(transfer.AggregateType, id)
	}
	w.mustDeliver(transaction.AggregateType, txnID)
	after, _ := w.store.counts()

	if after != before {
		t.Errorf("the log grew by %d events; the orchestrator must add nothing where the saga already ran", after-before)
	}
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Errorf("state = %v, want COMPLETED", got)
	}
}

// At-least-once delivery is only sufficient if redelivery is inert. Every
// message is delivered again, twice over, once the Transaction is done.
func TestOrchestrator_RedeliveringEveryMessageChangesNothing(t *testing.T) {
	t.Parallel()
	w, txnID, realID, shadowID := achDeposit(t)

	w.mustDeliver(transaction.AggregateType, txnID)
	w.settle(realID)
	w.mustDeliver(transfer.AggregateType, realID)
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Fatalf("state = %v, want COMPLETED before redelivery", got)
	}

	before, _ := w.store.counts()
	for range 2 {
		w.mustDeliver(transaction.AggregateType, txnID)
		w.mustDeliver(transfer.AggregateType, realID)
		w.mustDeliver(transfer.AggregateType, shadowID)
	}
	after, _ := w.store.counts()

	if after != before {
		t.Errorf("the log grew by %d events under redelivery, want 0", after-before)
	}
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Errorf("state = %v, want COMPLETED", got)
	}
}

// Two topics are two independent partitions however they are keyed, so their
// relative order is not a guarantee anything may lean on. The same set of
// triggers is delivered in several orders against a fresh world each time, and
// every one of them has to arrive at the same place.
func TestOrchestrator_ConvergesWhateverOrderTopicsArriveIn(t *testing.T) {
	t.Parallel()

	type step struct {
		aggregateType string
		which         string // "transaction", "real" or "shadow"
	}
	orders := map[string][]step{
		"transaction first": {
			{transaction.AggregateType, "transaction"},
			{transfer.AggregateType, "real"},
			{transfer.AggregateType, "shadow"},
		},
		"child before its transaction": {
			{transfer.AggregateType, "real"},
			{transfer.AggregateType, "shadow"},
			{transaction.AggregateType, "transaction"},
		},
		"stale transaction message interleaved": {
			{transaction.AggregateType, "transaction"},
			{transfer.AggregateType, "shadow"},
			{transaction.AggregateType, "transaction"},
			{transfer.AggregateType, "real"},
			{transaction.AggregateType, "transaction"},
		},
	}

	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, txnID, realID, shadowID := achDeposit(t)

			// The real leg is dispatched and settled first, so every message in
			// the orders above names an aggregate that genuinely exists — a
			// trigger can only ever follow the event that produced it.
			w.mustDeliver(transaction.AggregateType, txnID)
			w.settle(realID)

			ids := map[string]string{"transaction": txnID, "real": realID, "shadow": shadowID}
			for _, s := range order {
				id := ids[s.which]
				// The shadow leg does not exist until the real one is reconciled,
				// and a message for an aggregate with no stream is not something
				// the transport can produce. Skipping it keeps the order honest.
				if s.which == "shadow" && w.outcome(shadowID) == transfer.OutcomeNotFound {
					continue
				}
				if err := w.deliver(s.aggregateType, id); err != nil {
					t.Fatalf("Handle(%s %s) error = %v", s.aggregateType, s.which, err)
				}
			}

			if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
				t.Errorf("state = %v, want COMPLETED whatever the order", got)
			}
		})
	}
}

// A Transfer requested outside any Transaction has no owner to follow, and
// must be driven on its own rather than treated as an inconsistency.
func TestOrchestrator_DrivesAStandaloneTransferWithNoTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t, eventstore.NewMemoryStore(), ledger.NewFakeClient())
	from := testutil.ID("w1")
	to := testutil.ID("w2")
	w.openWallet(from, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	w.openWallet(to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	w.mintAndFundToken(from, testutil.ID("t1"), usd(1000))

	// Accepted and left there, so the trigger is what drives it.
	transferID := testutil.ID("xfer-standalone")
	if err := w.store.Append(context.Background(), transfer.AggregateType, transferID, 0, &transferpb.TransferRequestAccepted{
		Id: transferID, FromWalletId: from, ToWalletId: to, Amount: usd(400),
	}); err != nil {
		t.Fatalf("seed accepted: %v", err)
	}

	w.mustDeliver(transfer.AggregateType, transferID)

	if got := w.outcome(transferID); got != transfer.OutcomeCommitted {
		t.Errorf("Outcome() = %v, want committed", got)
	}
}

// A rejected request is published on the transfer topic like every other
// event, so the orchestrator is handed a trigger naming a Transfer whose saga
// never began. Nothing about that is exotic — insufficient capacity is an
// ordinary business answer — and the consumer commits no offset for a message
// its handler failed on, so treating a rejection as a failure would stop the
// orchestrator here and on every restart after it.
func TestOrchestrator_ARejectedRequestIsDrivenNoFurther(t *testing.T) {
	t.Parallel()
	w := newWorld(t, eventstore.NewMemoryStore(), ledger.NewFakeClient())
	from := testutil.ID("w1")
	to := testutil.ID("w2")
	w.openWallet(from, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	w.openWallet(to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	w.mintAndFundToken(from, testutil.ID("t1"), usd(100))

	// Rejected by the real server for the most ordinary reason there is: the
	// source Wallet cannot cover the amount.
	transferID := testutil.ID("xfer-rejected")
	resp, err := w.transfers.RequestTransfer(context.Background(), &transferpb.RequestTransferRequest{
		Id: transferID, FromWalletId: from, ToWalletId: to, Amount: usd(400),
	})
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if resp.GetTransferRequestRejected() == nil {
		t.Fatalf("result = %v, want a rejection to trigger on", resp.GetResult())
	}

	if err := w.deliver(transfer.AggregateType, transferID); err != nil {
		t.Fatalf("Handle(transfer %s) error = %v; a rejection must not halt the orchestrator", transferID, err)
	}

	if got := w.outcome(transferID); got != transfer.OutcomeRejected {
		t.Errorf("Outcome() = %v, want rejected", got)
	}
}

// stagedReversalRollback builds a Transaction whose staged real leg settles and
// completes, and whose shadow leg is then rejected outright because its source
// Wallet was never provisioned. Rollback therefore has to reverse a committed
// child, and because that child was staged, so is its Reversal — which parks,
// leaving the Transaction waiting on a second-generation aggregate.
func stagedReversalRollback(t *testing.T) (w *world, txnID, realID string) {
	t.Helper()
	w, bankAccount, cash, _, uncleared := achWorld(t)
	txnID = testutil.ID("txn-rollback")
	realID = testutil.ID("real")
	shadowID := testutil.ID("shadow")
	w.initialize(txnID,
		map[string]*transactionpb.Transfer{
			realID: {
				Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash,
				AutoProcess: true, Stage: true, MintSource: true,
			},
			shadowID: {
				Id: shadowID, Amount: usd(10000), FromWalletId: testutil.ID("never-provisioned"), ToWalletId: uncleared,
				AutoProcess: true, MintSource: true,
			},
		},
		map[string]*transactionpb.TransferIdList{shadowID: {TransferId: []string{realID}}},
	)

	w.mustDeliver(transaction.AggregateType, txnID)
	w.settle(realID)
	w.mustDeliver(transfer.AggregateType, realID)
	return w, txnID, realID
}

// The case go/docs/adr/0002 designed and this orchestrator has to deliver: a
// Reversal is a Transfer of its own, its terminal event arrives on the transfer
// topic like any other, and unless it is resolved back to the Transaction that
// asked for it, rollback parks forever.
func TestOrchestrator_AReversalsTerminalEventLandsItsTransactionOnRolledBack(t *testing.T) {
	t.Parallel()
	w, txnID, realID := stagedReversalRollback(t)

	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_ROLLBACK_STARTED {
		t.Fatalf("state = %v, want ROLLBACK_STARTED with a staged Reversal in flight", got)
	}

	reversalID := reversalRequestedFor(t, w, txnID, realID)
	if got := w.outcome(reversalID); got != transfer.OutcomeStaged {
		t.Fatalf("reversal Outcome() = %v, want staged: the wait state under test", got)
	}

	// The Reversal settles the same way the original did, and tells nobody.
	w.settle(reversalID)
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_ROLLBACK_STARTED {
		t.Fatalf("state = %v, want still ROLLBACK_STARTED before the trigger arrives", got)
	}

	// The Reversal's own trigger resolves to the Transaction that requested it.
	w.mustDeliver(transfer.AggregateType, reversalID)

	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_ROLLED_BACK {
		t.Errorf("state = %v, want ROLLED_BACK", got)
	}
}

// reversalRequestedFor reads back which Reversal the Transaction recorded
// itself as waiting on for transferID.
func reversalRequestedFor(t *testing.T, w *world, transactionID, transferID string) string {
	t.Helper()
	events, err := w.store.Load(context.Background(), transaction.AggregateType, transactionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, e := range events {
		if e.EventType != eventstore.EventType(&transactionpb.TransferReversalRequestedWithinTransaction{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rr, ok := msg.(*transactionpb.TransferReversalRequestedWithinTransaction); ok && rr.GetTransferId() == transferID {
			return rr.GetReversalId()
		}
	}
	t.Fatalf("no TransferReversalRequestedWithinTransaction recorded for %q", transferID)
	return ""
}

// Per-aggregate-type topics mean a transfer message and a transaction message
// can be handled at the same time and both append to one Transaction's stream.
// go/docs/adr/0001 called this out as newly reachable and asked for a test that
// provokes it rather than trusting the retry by inspection — so the store holds
// both handlers at the moment they have read the stream and not yet written to
// it, which makes the conflict certain rather than likely.
func TestOrchestrator_ConcurrentTriggersOnOneTransactionConverge(t *testing.T) {
	t.Parallel()
	w, txnID, realID, _ := achDeposit(t)

	w.mustDeliver(transaction.AggregateType, txnID)
	w.settle(realID)

	barrier := newAppendBarrier(transaction.AggregateType, txnID, 2)
	w.store.barrier = barrier

	var wg sync.WaitGroup
	errs := make([]error, 2)
	deliveries := []struct{ aggregateType, aggregateID string }{
		{transfer.AggregateType, realID},
		{transaction.AggregateType, txnID},
	}
	for i, d := range deliveries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = w.deliver(d.aggregateType, d.aggregateID)
		}()
	}
	wg.Wait()
	w.store.barrier = nil

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent delivery %d error = %v", i, err)
		}
	}
	if !barrier.tripped() {
		t.Fatal("the two handlers never wrote to the Transaction's stream at the same time; the race was not provoked")
	}
	if _, conflicts := w.store.counts(); conflicts == 0 {
		t.Error("no optimistic-concurrency conflict occurred; the test proved nothing about the retry")
	}
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Errorf("state = %v, want COMPLETED: concurrent appends must converge", got)
	}
}
