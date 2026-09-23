package transfer

import (
	"slices"
	"testing"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

// A Transfer never moves again once its stream holds one of these, so a
// reader of the whole log can skip it without folding it.
func TestTerminalEventTypes_AreExactlyTheEventsATransferNeverMovesPast(t *testing.T) {
	t.Parallel()
	got := TerminalEventTypes()

	for _, terminal := range []string{
		eventstore.EventType(&pb.TransferRequestRejected{}),
		eventstore.EventType(&pb.ReversalRequestRejected{}),
		eventstore.EventType(&pb.TransferCommitted{}),
		eventstore.EventType(&pb.TransferFailed{}),
		eventstore.EventType(&pb.TransferCancelled{}),
		eventstore.EventType(&pb.AcceptedTransferCancelled{}),
		eventstore.EventType(&pb.PreparedTransferCancelled{}),
	} {
		if !slices.Contains(got, terminal) {
			t.Errorf("TerminalEventTypes() is missing %s", terminal)
		}
	}
	for _, moving := range []string{
		eventstore.EventType(&pb.TransferRequestAccepted{}),
		eventstore.EventType(&pb.ReversalRequestAccepted{}),
		eventstore.EventType(&pb.TransferPrepared{}),
		eventstore.EventType(&pb.TransferStaged{}),
		eventstore.EventType(&pb.TransferPending{}),
	} {
		if slices.Contains(got, moving) {
			t.Errorf("TerminalEventTypes() includes %s, which a Transfer moves on from", moving)
		}
	}
}
