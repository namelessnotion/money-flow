package transaction

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/detid"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// eventIndex returns the position of the first event of msg's type recording
// transferID, or -1. Used to assert ordering between two bookkeeping facts on
// the Transaction's own stream.
func eventIndex(t *testing.T, events []eventstore.Event, wantType string, transferID string) int {
	t.Helper()
	for i, e := range events {
		if e.EventType != wantType {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if childEventTransferID(msg) == transferID {
			return i
		}
	}
	return -1
}

// reversalRequestedID returns the reversal_id recorded by transferID's
// TransferReversalRequestedWithinTransaction.
func reversalRequestedID(t *testing.T, events []eventstore.Event, transferID string) string {
	t.Helper()
	for _, e := range events {
		if e.EventType != eventstore.EventType(&pb.TransferReversalRequestedWithinTransaction{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rr, ok := msg.(*pb.TransferReversalRequestedWithinTransaction); ok && rr.GetTransferId() == transferID {
			return rr.GetReversalId()
		}
	}
	t.Fatalf("no TransferReversalRequestedWithinTransaction found for %q", transferID)
	return ""
}

// reversingRollbackFixture builds a Transaction whose "real" child commits and
// whose "shadow" child is rejected at accept time, forcing rollback to reverse
// the committed child. Returns the store, the transaction id and the real
// child's id.
func reversingRollbackFixture(t *testing.T, lc ledger.Client) (eventstore.Store, string, string) {
	t.Helper()
	store := eventstore.NewMemoryStore()
	bankAccount := testutil.ID("bank-account")
	cash := testutil.ID("cash")
	uncleared := testutil.ID("uncleared")
	neverProvisioned := testutil.ID("bank-control-not-provisioned")
	openWallet(t, store, bankAccount, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, cash, sharedpb.Allows_ALLOWS_NONE)
	openWallet(t, store, uncleared, sharedpb.Allows_ALLOWS_NONE)

	txnServer := NewServer(store, newTransferServer(store, lc))
	txnID := testutil.ID("txn-rev")
	realID := testutil.ID("real")
	shadowID := testutil.ID("shadow")
	if _, err := txnServer.StartInitializingTransaction(context.Background(), &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			realID:   {Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash, AutoProcess: true, MintSource: true},
			shadowID: {Id: shadowID, Amount: usd(10000), FromWalletId: neverProvisioned, ToWalletId: uncleared, AutoProcess: true, MintSource: true},
		},
		TransferDependency: map[string]*pb.TransferIdList{shadowID: {TransferId: []string{realID}}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	return store, txnID, realID
}

// TestReversingRollback_RecordsReversalRequestedBeforeRolledBack proves the
// two facts are separate and ordered: the Transaction records which Reversal it
// is waiting on before it records the child as rolled back. Under the
// synchronous saga both land in one call, but the ordering is what lets the
// second one be deferred to a later trigger after the async cutover.
func TestReversingRollback_RecordsReversalRequestedBeforeRolledBack(t *testing.T) {
	t.Parallel()
	store, txnID, realID := reversingRollbackFixture(t, ledger.NewFakeClient())

	events, err := store.Load(context.Background(), AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	requested := eventIndex(t, events, eventstore.EventType(&pb.TransferReversalRequestedWithinTransaction{}), realID)
	if requested < 0 {
		t.Fatal("no TransferReversalRequestedWithinTransaction recorded for the committed child")
	}
	rolledBack := eventIndex(t, events, eventstore.EventType(&pb.TransferRolledBackWithinTransaction{}), realID)
	if rolledBack < 0 {
		t.Fatal("no TransferRolledBackWithinTransaction recorded for the committed child")
	}
	if requested >= rolledBack {
		t.Errorf("reversal requested at index %d, rolled back at %d; want requested first", requested, rolledBack)
	}

	// The recorded reversal id must be the deterministic one, and must match
	// what the terminal rollback fact reports as its detail.
	wantID := detid.New(txnID + ":reversal:" + realID)
	if got := reversalRequestedID(t, events, realID); got != wantID {
		t.Errorf("recorded reversal_id = %q, want the deterministic %q", got, wantID)
	}
	if _, detail := rolledBackMethod(t, events, realID); detail != wantID {
		t.Errorf("rolled-back detail_id = %q, want %q", detail, wantID)
	}
}

// TestReversingRollback_ChildIsRollbackRequestedWhileReversalIsInFlight is the
// case the synchronous implementation had no vocabulary for. The stream is
// truncated to the moment just after the reversal was requested — exactly what
// an async orchestrator sees between the request and the reversal's own
// terminal event — and the fold must report the child as in flight, not as
// rolled back and not as untouched.
func TestReversingRollback_ChildIsRollbackRequestedWhileReversalIsInFlight(t *testing.T) {
	t.Parallel()
	store, txnID, realID := reversingRollbackFixture(t, ledger.NewFakeClient())
	events, err := store.Load(context.Background(), AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	cut := eventIndex(t, events, eventstore.EventType(&pb.TransferReversalRequestedWithinTransaction{}), realID)
	if cut < 0 {
		t.Fatal("no TransferReversalRequestedWithinTransaction recorded")
	}
	children, err := foldChildStates(events[:cut+1])
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if children[realID] != childRollbackRequested {
		t.Errorf("child state = %v, want childRollbackRequested", children[realID])
	}
}

// TestReadyToRollback_SkipsChildWhoseReversalIsInFlight: a child whose reversal
// has been requested but not resolved must not be picked again by a later
// sweep, or the sweep re-issues RequestReversal on every pass.
func TestReadyToRollback_SkipsChildWhoseReversalIsInFlight(t *testing.T) {
	t.Parallel()
	transfers := map[string]*pb.Transfer{"a": {Id: "a"}, "b": {Id: "b"}}
	deps := map[string]*pb.TransferIdList{}
	touched := map[string]bool{"a": true, "b": true}
	rolledBack := map[string]bool{}
	inFlight := map[string]bool{"a": true}

	got := readyToRollback(transfers, deps, touched, rolledBack, inFlight)
	for _, id := range got {
		if id == "a" {
			t.Errorf("readyToRollback() = %v, must not include the in-flight child %q", got, "a")
		}
	}
	if len(got) != 1 || got[0] != "b" {
		t.Errorf("readyToRollback() = %v, want only [b]", got)
	}
}

// TestReversingRollback_FailedReversalLandsOnRollbackFailed: once the inline
// confirmation is gone, a reversal that does not commit is the only route to
// TransactionRollbackFailed. The ledger rejects every transfer after the
// committed child's own, so the reversal fails.
func TestReversingRollback_FailedReversalLandsOnRollbackFailed(t *testing.T) {
	t.Parallel()
	lc := &rejectingAfterNCallsClient{Client: ledger.NewFakeClient(), n: 1}
	store, txnID, realID := reversingRollbackFixture(t, lc)

	ctx := context.Background()
	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateRollbackFailed {
		t.Fatalf("transaction state = %v, want rollback_failed", got)
	}
	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if children[realID] != childRollbackFailed {
		t.Errorf("child state = %v, want childRollbackFailed", children[realID])
	}

	// It must still have recorded which reversal it was waiting on, and that
	// reversal must genuinely not have committed.
	revID := reversalRequestedID(t, events, realID)
	if outcome, err := transfer.Outcome(ctx, store, revID); err != nil {
		t.Fatalf("Outcome() error = %v", err)
	} else if outcome == transfer.OutcomeCommitted {
		t.Errorf("reversal %q committed; the test fixture failed to make it fail", revID)
	}
}
