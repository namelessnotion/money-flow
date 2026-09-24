package transaction

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// Since the async cutover, Initialized is a state a Transaction sits in until
// the orchestrator folds its TransactionInitialized trigger. The RPCs a caller
// may reasonably make right after accepting one must not answer differently
// depending on how far behind publication happens to be.

func initializedWorld(t *testing.T) (store eventstore.Store, lc *ledger.FakeClient, w1, w2 string) {
	t.Helper()
	memory := eventstore.NewMemoryStore()
	lc = ledger.NewFakeClient()
	w1, w2 = testutil.ID("w1"), testutil.ID("w2")
	openWallet(t, memory, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, memory, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintAndFundToken(t, memory, lc, w1, testutil.ID("t1"), usd(1000))
	return memory, lc, w1, w2
}

func TestStartTransactionRollback_RollsBackATransactionThatHasNotStartedYet(t *testing.T) {
	t.Parallel()
	store, lc, w1, w2 := initializedWorld(t)
	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID, rootID := testutil.ID("txn1"), testutil.ID("root")
	if _, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id:        txnID,
		Transfers: map[string]*pb.Transfer{rootID: {Id: rootID, Amount: usd(100), FromWalletId: w1, ToWalletId: w2}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	// No fold has happened: the Transaction is still Initialized.
	resp, err := txnServer.StartTransactionRollback(ctx, &pb.StartTransactionRollbackRequest{Id: txnID, Reason: "returned"})
	if err != nil {
		t.Fatalf("StartTransactionRollback() error = %v, want the rollback recorded", err)
	}
	if resp.GetState() != pb.TransactionState_TRANSACTION_STATE_ROLLBACK_STARTED {
		t.Fatalf("state = %v, want ROLLBACK_STARTED", resp.GetState())
	}

	driveSaga(t, txnServer, xferServer, store, txnID)
	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateRolledBack {
		t.Errorf("state = %v, want rolled_back", got)
	}
	if outcome, err := transfer.Outcome(ctx, store, rootID); err != nil || outcome != transfer.OutcomeNotFound {
		t.Errorf("root Outcome() = (%v, %v), want not_found: a Transaction rolled back before it started dispatches nothing", outcome, err)
	}
}

// rollbackLandsDuringStartStore makes a StartTransactionRollback land between
// the orchestrator folding a Transaction as Initialized and appending
// TransactionStarted — exactly the window that allowing rollback from
// Initialized opens.
type rollbackLandsDuringStartStore struct {
	eventstore.Store
	landed bool
	err    error
}

func (s *rollbackLandsDuringStartStore) Append(
	ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message,
) error {
	if _, starting := firstEvent(events).(*pb.TransactionStarted); starting && !s.landed {
		s.landed = true
		s.err = s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq,
			&pb.TransactionRollbackStarted{Id: aggregateID, Reason: "returned"})
	}
	return s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
}

func firstEvent(events []proto.Message) proto.Message {
	if len(events) == 0 {
		return nil
	}
	return events[0]
}

// Starting is a decision made from the Transaction's state, so it may only be
// recorded against the state it was decided from. Appended after a rollback
// it would put the Transaction back to Started, undo the rollback without a
// trace, and dispatch the children the caller had just asked to abandon.
func TestRunSaga_DoesNotStartATransactionThatWasRolledBackWhileStarting(t *testing.T) {
	t.Parallel()
	memory, lc, w1, w2 := initializedWorld(t)
	store := &rollbackLandsDuringStartStore{Store: memory}
	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID, rootID := testutil.ID("txn1"), testutil.ID("root")
	if _, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id:        txnID,
		Transfers: map[string]*pb.Transfer{rootID: {Id: rootID, Amount: usd(100), FromWalletId: w1, ToWalletId: w2}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	if err := txnServer.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !store.landed || store.err != nil {
		t.Fatalf("the rollback never landed mid-start (landed=%v, err=%v); the test proved nothing", store.landed, store.err)
	}

	events, err := memory.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateRolledBack {
		t.Errorf("state = %v, want rolled_back", got)
	}
	for _, e := range events {
		if e.EventType == eventstore.EventType(&pb.TransactionStarted{}) {
			t.Errorf("TransactionStarted was recorded after the rollback began")
		}
	}
	if outcome, err := transfer.Outcome(ctx, memory, rootID); err != nil || outcome != transfer.OutcomeNotFound {
		t.Errorf("root Outcome() = (%v, %v), want not_found", outcome, err)
	}
}
