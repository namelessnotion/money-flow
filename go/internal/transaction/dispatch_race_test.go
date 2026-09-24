package transaction

import (
	"context"
	"testing"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// ADR 0001 lets a transfer-topic run and a transaction-topic run fold the same
// Transaction at once. The tests below hold one run inside its RequestTransfer
// call for a chosen child, after that child's Transfer has accepted, until a
// concurrent rollback has run to its conclusion. A dispatch that is recorded
// only after its side effect then lands after the terminal, and the Transfer
// it started commits under a RolledBack Transaction (go/docs/adr/0011).

// rollbackDuringRequestClient is the real transfer.Server, except that the
// first RequestTransfer for target runs meanwhile before returning: the
// concurrent run, held to its conclusion while the dispatching run waits.
type rollbackDuringRequestClient struct {
	*transfer.Server
	target    string
	fired     bool
	meanwhile func()
}

func (c *rollbackDuringRequestClient) RequestTransfer(ctx context.Context, req *transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error) {
	resp, err := c.Server.RequestTransfer(ctx, req)
	if err == nil && req.GetId() == c.target && !c.fired {
		c.fired = true
		c.meanwhile()
	}
	return resp, err
}

// requireRolledBackWithout asserts the Transaction ended RolledBack with
// nothing recorded after it, and that childID's Transfer — driven for as long
// as its own saga would be — never committed.
func requireRolledBackWithout(t *testing.T, store eventstore.Store, xfers *transfer.Server, txnID, childID string) {
	t.Helper()
	ctx := context.Background()

	// Whatever the Transfer's own saga would do from here, it gets to do: the
	// orchestrator drives any Transfer whose acceptance was published.
	if err := xfers.Resume(ctx, childID); err != nil {
		t.Fatalf("Resume(transfer %s) error = %v", childID, err)
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateRolledBack {
		t.Fatalf("state = %v, want rolled_back; events = %v", got, eventTypesOf(events))
	}
	if last := events[len(events)-1].EventType; last != eventstore.EventType(&pb.TransactionRolledBack{}) {
		t.Errorf("stream ends with %s, want TransactionRolledBack last: nothing may be recorded after a terminal; events = %v",
			last, eventTypesOf(events))
	}
	switch outcome, err := transfer.Outcome(ctx, store, childID); {
	case err != nil:
		t.Fatalf("Outcome(%s) error = %v", childID, err)
	case outcome != transfer.OutcomeNotFound && outcome != transfer.OutcomeCancelled && outcome != transfer.OutcomeRejected:
		t.Errorf("child %s Outcome() = %v under a rolled-back Transaction, want it never requested or cancelled", childID, outcome)
	}
}

// The interleaving as first suspected: child A fails while the run that
// dispatched B sits between B's RequestTransfer and recording it. A is a
// separate earlier root, and B waits on a root C, so B is the only child that
// run dispatches and the order within its slice cannot hide the race.
func TestDispatch_AChildFailingMidDispatchRollsBackTheChildBeingRequested(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, _, _ := achWallets(t, store)
	xfers := newTransferServer(store, lc)
	ctx := context.Background()

	txnID := testutil.ID("txn-child-fails")
	aID, bID, cID := testutil.ID("a"), testutil.ID("b"), testutil.ID("c")
	client := &rollbackDuringRequestClient{Server: xfers, target: bID}
	txns := NewServer(store, client)
	client.meanwhile = func() {
		// A fails on its own, the way a failed prepare would leave it, and the
		// transfer-topic run that follows the link resumes the Transaction.
		if _, err := xfers.CancelAcceptedTransfer(ctx, &transferpb.CancelAcceptedTransferRequest{Id: aID, Reason: "failed"}); err != nil {
			t.Fatalf("CancelAcceptedTransfer(a) error = %v", err)
		}
		driveSaga(t, txns, xfers, store, txnID)
	}

	if _, err := txns.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			aID: {Id: aID, Amount: usd(100), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true},
			cID: {Id: cID, Amount: usd(100), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true},
			bID: {Id: bID, Amount: usd(100), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true},
		},
		TransferDependency: map[string]*pb.TransferIdList{bID: {TransferId: []string{cID}}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	// A and C are requested; only C's saga runs, so A is still in flight.
	if err := txns.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if err := xfers.Resume(ctx, cID); err != nil {
		t.Fatalf("Resume(c) error = %v", err)
	}

	// C's commit wakes the Transaction, which records it and dispatches B.
	if err := txns.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !client.fired {
		t.Fatalf("B was never requested; the test proved nothing")
	}
	requireRolledBackWithout(t, store, xfers, txnID, bID)
}

// The same window, with an operator's StartTransactionRollback as the
// concurrent decision. Both roots share one slice, in whichever order.
func TestDispatch_ARollbackRequestedMidDispatchRollsBackTheChildBeingRequested(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, _, _ := achWallets(t, store)
	xfers := newTransferServer(store, lc)
	ctx := context.Background()

	txnID := testutil.ID("txn-operator-rollback")
	aID, bID := testutil.ID("a"), testutil.ID("b")
	client := &rollbackDuringRequestClient{Server: xfers, target: bID}
	txns := NewServer(store, client)
	client.meanwhile = func() {
		if _, err := txns.StartTransactionRollback(ctx, &pb.StartTransactionRollbackRequest{Id: txnID, Reason: "returned"}); err != nil {
			t.Fatalf("StartTransactionRollback() error = %v", err)
		}
		driveSaga(t, txns, xfers, store, txnID)
	}

	if _, err := txns.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			aID: {Id: aID, Amount: usd(100), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true},
			bID: {Id: bID, Amount: usd(100), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true},
		},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	if err := txns.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !client.fired {
		t.Fatalf("B was never requested; the test proved nothing")
	}
	requireRolledBackWithout(t, store, xfers, txnID, bID)
	requireRolledBackWithout(t, store, xfers, txnID, aID)
}

// seedIntendedChild writes a Transaction that recorded its intent to request
// childID and then lost its driver before making the call: the child is
// Requested on the Transaction's stream and has no stream of its own.
func seedIntendedChild(t *testing.T, store eventstore.Store, txnID string, child *pb.Transfer) {
	t.Helper()
	if err := store.Append(context.Background(), AggregateType, txnID, 0,
		&pb.TransactionInitialized{Id: txnID, FactoryName: "dispatch_race_test", FactoryVersion: "1", Transfers: map[string]*pb.Transfer{child.GetId(): child}},
		&pb.TransactionStarted{Id: txnID},
		&pb.TransferRequestedWithinTransaction{Id: txnID, TransferId: child.GetId()},
	); err != nil {
		t.Fatalf("seed intended child: %v", err)
	}
}

// An intent whose request never happened is finished by the next forward run,
// not left waiting on a Transfer that nothing will ever create.
func TestResume_RequestsAChildWhoseIntentOutlivedItsDriver(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, _, _ := achWallets(t, store)
	xfers := newTransferServer(store, lc)
	txns := NewServer(store, xfers)

	txnID, bID := testutil.ID("txn-intent-forward"), testutil.ID("b")
	seedIntendedChild(t, store, txnID, &pb.Transfer{Id: bID, Amount: usd(100), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true})

	driveSaga(t, txns, xfers, store, txnID)
	events, err := store.Load(context.Background(), AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateCompleted {
		t.Fatalf("state = %v, want completed; events = %v", got, eventTypesOf(events))
	}
}

// Rolling back an intent whose request never happened leaves no Transfer that
// could still commit.
func TestRollback_ResolvesAChildWhoseIntentOutlivedItsDriver(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, _, _ := achWallets(t, store)
	xfers := newTransferServer(store, lc)
	txns := NewServer(store, xfers)
	ctx := context.Background()

	txnID, bID := testutil.ID("txn-intent-rollback"), testutil.ID("b")
	seedIntendedChild(t, store, txnID, &pb.Transfer{Id: bID, Amount: usd(100), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true})

	if _, err := txns.StartTransactionRollback(ctx, &pb.StartTransactionRollbackRequest{Id: txnID, Reason: "returned"}); err != nil {
		t.Fatalf("StartTransactionRollback() error = %v", err)
	}
	driveSaga(t, txns, xfers, store, txnID)
	requireRolledBackWithout(t, store, xfers, txnID, bID)
}

// A child outcome decided from a fold that a rollback has since overtaken is
// moot: the rollback reads the child's live outcome itself. Recorded anyway,
// it would land after the terminal and rewrite a rolled-back child as
// completed.
func TestAppendSagaStep_DropsAChildOutcomeOnceTheTransactionHasConcluded(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	txns := NewServer(store, nil)
	ctx := context.Background()

	txnID, bID := testutil.ID("txn-late-outcome"), testutil.ID("b")
	if err := store.Append(ctx, AggregateType, txnID, 0,
		&pb.TransactionInitialized{Id: txnID, FactoryName: "dispatch_race_test", FactoryVersion: "1", Transfers: map[string]*pb.Transfer{
			bID: {Id: bID, Amount: usd(100), FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2")},
		}},
		&pb.TransactionStarted{Id: txnID},
		&pb.TransferRequestedWithinTransaction{Id: txnID, TransferId: bID},
		&pb.TransactionRollbackStarted{Id: txnID, Reason: "returned"},
		&pb.TransferRolledBackWithinTransaction{Id: txnID, TransferId: bID, Method: pb.RollbackMethod_ROLLBACK_METHOD_CANCELLED},
		&pb.TransactionRolledBack{Id: txnID, Reason: "returned"},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := txns.appendSagaStep(ctx, txnID, &pb.TransferCompletedWithinTransaction{Id: txnID, TransferId: bID}); err != nil {
		t.Fatalf("appendSagaStep() error = %v", err)
	}
	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if last := events[len(events)-1].EventType; last != eventstore.EventType(&pb.TransactionRolledBack{}) {
		t.Errorf("stream ends with %s, want TransactionRolledBack last; events = %v", last, eventTypesOf(events))
	}
}

// A rollback decided from Started — an operator's, read before the last child
// completed — must not land once the Transaction has completed: nothing rolls
// back from Completed, and recorded anyway it would reopen a closed Transaction.
func TestAppendSagaStep_DropsARollbackDecidedBeforeTheTransactionCompleted(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	txns := NewServer(store, nil)
	ctx := context.Background()

	txnID, bID := testutil.ID("txn-late-rollback"), testutil.ID("b")
	if err := store.Append(ctx, AggregateType, txnID, 0,
		&pb.TransactionInitialized{Id: txnID, FactoryName: "dispatch_race_test", FactoryVersion: "1", Transfers: map[string]*pb.Transfer{
			bID: {Id: bID, Amount: usd(100), FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2")},
		}},
		&pb.TransactionStarted{Id: txnID},
		&pb.TransferRequestedWithinTransaction{Id: txnID, TransferId: bID},
		&pb.TransferCompletedWithinTransaction{Id: txnID, TransferId: bID},
		&pb.TransactionCompleted{Id: txnID},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Decided from Started, recorded after the completion.
	if err := txns.appendSagaStep(ctx, txnID, &pb.TransactionRollbackStarted{Id: txnID, Reason: "returned"}); err != nil {
		t.Fatalf("appendSagaStep() error = %v", err)
	}
	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateCompleted {
		t.Errorf("state = %v, want completed; events = %v", got, eventTypesOf(events))
	}
}
