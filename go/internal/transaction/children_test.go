package transaction

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// Every Transfer the Transaction asked for has a stream of its own and so can
// be woken; a gated child was never asked for and has none.
func TestChildTransferIDs_ListsRequestedChildrenAndReversalsOnly(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	txnID := testutil.ID("txn1")
	a, gated, reversal := testutil.ID("a"), testutil.ID("gated"), testutil.ID("reversal")

	if err := store.Append(ctx, AggregateType, txnID, 0,
		&pb.TransactionInitialized{Id: txnID},
		&pb.TransactionStarted{Id: txnID},
		&pb.TransferRequestedWithinTransaction{Id: txnID, TransferId: a},
		&pb.TransferGatedWithinTransaction{Id: txnID, TransferId: gated},
		&pb.TransferCompletedWithinTransaction{Id: txnID, TransferId: a},
		&pb.TransactionRollbackStarted{Id: txnID},
		&pb.TransferReversalRequestedWithinTransaction{Id: txnID, TransferId: a, ReversalId: reversal},
	); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, err := ChildTransferIDs(ctx, store, txnID)
	if err != nil {
		t.Fatalf("ChildTransferIDs() error = %v", err)
	}
	if want := []string{a, reversal}; !slices.Equal(got, want) {
		t.Errorf("ChildTransferIDs() = %v, want %v", got, want)
	}
}

// Each listed type must fold to a state runSaga stops at for good; one that
// did not would have cmd/resume -open skip a Transaction still in flight.
func TestTerminalEventTypes_EachFoldsToATrueTerminal(t *testing.T) {
	t.Parallel()
	byType := map[string]proto.Message{}
	for _, m := range []proto.Message{
		&pb.TransactionCompleted{}, &pb.TransactionRolledBack{}, &pb.TransactionRollbackFailed{}, &pb.TransactionRejected{},
	} {
		byType[eventstore.EventType(m)] = m
	}

	got := TerminalEventTypes()
	if len(got) != len(byType) {
		t.Errorf("TerminalEventTypes() = %v, want exactly %d types", got, len(byType))
	}
	for _, eventType := range got {
		if _, ok := byType[eventType]; !ok {
			t.Errorf("TerminalEventTypes() includes %s, which is not a terminal Transaction event", eventType)
		}
		events := []eventstore.Event{
			{EventType: eventstore.EventType(&pb.TransactionInitialized{})},
			{EventType: eventstore.EventType(&pb.TransactionStarted{})},
			{EventType: eventType},
		}
		switch state := topLevelState(events); state {
		case stateCompleted, stateRolledBack, stateRollbackFailed, stateRejected:
		default:
			t.Errorf("%s folds to %v, want a terminal state", eventType, state)
		}
	}
}
