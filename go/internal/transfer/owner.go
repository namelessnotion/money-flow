package transfer

import (
	"context"

	"github.com/twitchtv/twirp"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

// OwningTransaction reads which Transaction, if any, drove transferID (a
// Transfer's or a Reversal's — same aggregate type) off the Accepted event
// that opens its stream. Like Outcome, this is the read-only cross-aggregate
// view Transaction's saga needs — here so that a trigger naming a Transfer can
// be resolved to the Transaction whose own fold has to run — without exposing
// transfer's own unexported state.
//
// Read-only: it never drives the saga forward, only reports a link the log
// already recorded. The answer is stable for the life of the stream, since it
// is taken from the first event and no later event can change it.
//
// Three distinct answers collapse into "" deliberately, because every caller
// treats them the same way — there is no Transaction to re-fold:
//
//   - the stream does not exist;
//   - the Transfer is standalone, requested outside any Transaction;
//   - the request was rejected. The Rejected events carry no transaction_id,
//     and need none: requestChildTransfer records an accept-time rejection
//     onto the Transaction's own stream in the same call, so the Transaction
//     is woken by its own event rather than by the child's.
func OwningTransaction(ctx context.Context, store eventstore.Store, transferID string) (string, error) {
	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return "", twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return "", nil
	}
	switch events[0].EventType {
	case eventstore.EventType(&pb.TransferRequestAccepted{}), eventstore.EventType(&pb.ReversalRequestAccepted{}):
	default:
		return "", nil
	}

	msg, err := events[0].Decode()
	if err != nil {
		return "", twirp.InternalErrorWith(err)
	}
	switch m := msg.(type) {
	case *pb.TransferRequestAccepted:
		return m.GetTransactionId(), nil
	case *pb.ReversalRequestAccepted:
		return m.GetTransactionId(), nil
	default:
		return "", nil
	}
}
