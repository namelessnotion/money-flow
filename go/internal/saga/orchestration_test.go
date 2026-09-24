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
				Stage: true, MintSource: true,
			},
			shadowID: {
				Id: shadowID, Amount: usd(10000), FromWalletId: bankControl, ToWalletId: uncleared,
				Stage: false, MintSource: true,
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

	// The transaction topic starts it: initialized -> started, and the real leg
	// is requested. Requested is all it is — accepting a Transfer no longer runs
	// it, so it has not staged yet.
	w.mustDeliver(transaction.AggregateType, txnID)
	if got := w.outcome(realID); got != transfer.OutcomeInFlight {
		t.Fatalf("real Outcome() = %v, want in_flight: accepted, not yet run", got)
	}
	if got := w.outcome(shadowID); got != transfer.OutcomeNotFound {
		t.Fatalf("shadow Outcome() = %v, want not_found: it depends on real completing", got)
	}

	// The real leg's own acceptance is a trigger too, and folding it is what
	// prepares and stages it.
	w.mustDeliver(transfer.AggregateType, realID)
	if got := w.outcome(realID); got != transfer.OutcomeStaged {
		t.Fatalf("real Outcome() = %v, want staged", got)
	}

	// ...days pass, ACH settles. The Transaction is told nothing.
	w.settle(realID)
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_STARTED {
		t.Fatalf("state = %v, want still STARTED before the trigger arrives", got)
	}

	// The transfer topic carries the settlement back: the committed child is
	// reconciled and the shadow leg becomes ready, so it is requested.
	w.mustDeliver(transfer.AggregateType, realID)
	if got := w.outcome(shadowID); got != transfer.OutcomeInFlight {
		t.Fatalf("shadow Outcome() = %v, want in_flight: requested by the reconciliation", got)
	}
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_STARTED {
		t.Fatalf("state = %v, want still STARTED: the shadow leg has not run", got)
	}

	// And the shadow leg's own trigger runs it and, following the link to its
	// owner, completes the Transaction.
	w.mustDeliver(transfer.AggregateType, shadowID)
	if got := w.outcome(shadowID); got != transfer.OutcomeCommitted {
		t.Errorf("shadow Outcome() = %v, want committed", got)
	}
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Fatalf("state = %v, want COMPLETED", got)
	}
}

// At-least-once delivery is only sufficient if redelivery is inert. Every
// message is delivered again, twice over, once the Transaction is done.
func TestOrchestrator_RedeliveringEveryMessageChangesNothing(t *testing.T) {
	t.Parallel()
	w, txnID, realID, shadowID := achDeposit(t)

	w.mustDeliver(transaction.AggregateType, txnID)
	w.mustDeliver(transfer.AggregateType, realID)
	w.settle(realID)
	w.drain(txnID)
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
			w.mustDeliver(transfer.AggregateType, realID)
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

			// The order above is the thing under test; what follows is simply
			// the rest of the messages arriving, which at-least-once delivery
			// guarantees they eventually do. Convergence is the claim, so an
			// order that has merely not finished yet is not a failure — one
			// that cannot finish is.
			w.drain(txnID)

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
				Stage: true, MintSource: true,
			},
			shadowID: {
				Id: shadowID, Amount: usd(10000), FromWalletId: testutil.ID("never-provisioned"), ToWalletId: uncleared,
				MintSource: true,
			},
		},
		map[string]*transactionpb.TransferIdList{shadowID: {TransferId: []string{realID}}},
	)

	w.mustDeliver(transaction.AggregateType, txnID)
	// One trigger per hop: the real leg's acceptance stages it, the settlement
	// reconciles it and requests the shadow leg, the shadow leg's own trigger
	// runs it and it fails, and that failure starts the rollback and requests
	// the Reversal — which then needs a trigger of its own to stage.
	w.mustDeliver(transfer.AggregateType, realID)
	w.settle(realID)
	w.drain(txnID)
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
	// Staging the real leg is its own hop now, and it has to have happened
	// before the settlement below means anything.
	w.mustDeliver(transfer.AggregateType, realID)
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
	// Both handlers raced to reconcile the settled child and dispatch the
	// shadow leg; running that leg is one more hop, and convergence is the
	// claim under test rather than the message count.
	w.drain(txnID)
	if got := w.transactionState(txnID); got != transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Errorf("state = %v, want COMPLETED: concurrent appends must converge", got)
	}
}

// TestOrchestrator_ConcurrentDispatchOfTheSameReadyChildConverges checks the
// transaction-side half of go/docs/adr/0005's question: requestChildTransfer
// (dispatchReady's per-child call) has no claim of its own, unlike
// transfer.Server's stage()/commit()/etc. This is deliberate, not an
// oversight — verified here rather than only argued in the ADR. Two
// concurrent drivers reaching dispatchReady for the same freshly-initialized
// Transaction both see the same ready child and both call
// transfer.RequestTransfer for it, but that's safe by composition: transfer.
// RequestTransfer is itself idempotent by transferID (and, after this
// decision's transfer-side fix, its own stage()/commit() converge under
// concurrent entry too), and transaction's own appendSagaStep dedupes by
// (event type, transfer_id) across the whole stream, not just its tail — so
// both callers recording the same child's outcome converge without a
// separate claim being needed here.
func TestOrchestrator_ConcurrentDispatchOfTheSameReadyChildConverges(t *testing.T) {
	t.Parallel()
	w, txnID, realID, _ := achDeposit(t)

	barrier := newAppendBarrier(transaction.AggregateType, txnID, 2)
	w.store.barrier = barrier

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = w.deliver(transaction.AggregateType, txnID)
		}(i)
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

	events, err := w.store.Load(context.Background(), transaction.AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	recorded := 0
	for _, e := range events {
		if e.EventType != "transaction.v1.TransferRequestedWithinTransaction" && e.EventType != "transaction.v1.TransferFailedWithinTransaction" {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		var childID string
		switch m := msg.(type) {
		case *transactionpb.TransferRequestedWithinTransaction:
			childID = m.GetTransferId()
		case *transactionpb.TransferFailedWithinTransaction:
			childID = m.GetTransferId()
		}
		if childID == realID {
			recorded++
		}
	}
	if recorded != 1 {
		t.Errorf("%d events recorded %s's dispatch outcome, want exactly 1", recorded, realID)
	}

	// The underlying Transfer itself must also have converged cleanly
	// (staged, per achDeposit's realID spec) rather than having hit the
	// transfer-side contradiction this whole decision exists to prevent. Its
	// own trigger is what runs it that far — the racing dispatches above only
	// accepted it, once.
	if err := w.deliver(transfer.AggregateType, realID); err != nil {
		t.Fatalf("Handle(transfer %s) error = %v", realID, err)
	}
	if outcome, err := transfer.Outcome(context.Background(), w.store, realID); err != nil || outcome != transfer.OutcomeStaged {
		t.Errorf("transfer.Outcome(%s) = (%v, %v), want (OutcomeStaged, nil)", realID, outcome, err)
	}
}
