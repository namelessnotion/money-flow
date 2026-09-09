package transfer

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// A Transfer accepted but never advanced — the state an external driver finds
// it in once nothing runs the saga in the accepting call — is driven to its
// terminal state by Resume alone.
func TestResume_DrivesAnAcceptedTransferToCommitted(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	ctx := context.Background()
	transferID := testutil.ID("xfer1")
	if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
		Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400),
	}); err != nil {
		t.Fatalf("seed accepted: %v", err)
	}

	server := NewServer(store, lc, nil, nil)
	if err := server.Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	if got, err := Outcome(ctx, store, transferID); err != nil || got != OutcomeCommitted {
		t.Fatalf("Outcome() = (%v, %v), want OutcomeCommitted", got, err)
	}
}

// Resume is the orchestrator's whole vocabulary, so it has to be safe to call
// on a Transfer that is already finished: a redelivered trigger must change
// neither the state nor the length of the stream.
func TestResume_IsANoOpOnATerminalTransfer(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	transferID := testutil.ID("xfer1")
	if _, err := server.RequestTransfer(ctx, transferRequest(transferID, testutil.ID("w1"), testutil.ID("w2"), usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	before, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if err := server.Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	after, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("stream grew from %d to %d events; Resume must be a no-op here", len(before), len(after))
	}
}

// A trigger always names an aggregate the log already holds, so an id with no
// stream is a real inconsistency and must surface as one rather than be
// silently treated as done.
func TestResume_ErrorsOnAnUnknownID(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, ledger.NewFakeClient(), nil, nil)

	if err := server.Resume(context.Background(), testutil.ID("never-existed")); err == nil {
		t.Error("Resume() on an unknown id = nil, want an error")
	}
}

// An ordinary business rejection — insufficient capacity, an unknown Wallet, a
// closed Transaction — is recorded on the Transfer's own stream and published
// like any other event, so the orchestrator will deliver a trigger naming it.
// The request never entered the saga, so there is nothing to drive: Resume has
// to settle for doing nothing rather than failing, which under the consumer's
// retry-then-halt policy would stop the orchestrator on a message it can never
// get past.
func TestResume_IsANoOpOnARejectedRequest(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	transferID := testutil.ID("xfer1")
	if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestRejected{
		Id: transferID, Reason: "insufficient capacity",
	}); err != nil {
		t.Fatalf("seed rejected: %v", err)
	}

	server := NewServer(store, ledger.NewFakeClient(), nil, nil)
	if err := server.Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Errorf("stream grew to %d events; a rejected request must stay as it is", len(events))
	}
	if got, err := Outcome(ctx, store, transferID); err != nil || got != OutcomeRejected {
		t.Fatalf("Outcome() = (%v, %v), want OutcomeRejected", got, err)
	}
}

// A Reversal's rejection opens its stream the same way a Transfer's does, on
// the same aggregate type, and reaches the orchestrator down the same topic.
func TestResume_IsANoOpOnARejectedReversal(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	reversalID := testutil.ID("rev1")
	if err := store.Append(ctx, AggregateType, reversalID, 0, &pb.ReversalRequestRejected{
		Id: reversalID, Reason: "transfer not reversible",
	}); err != nil {
		t.Fatalf("seed rejected: %v", err)
	}

	server := NewServer(store, ledger.NewFakeClient(), nil, nil)
	if err := server.Resume(ctx, reversalID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	if got, err := Outcome(ctx, store, reversalID); err != nil || got != OutcomeRejected {
		t.Fatalf("Outcome() = (%v, %v), want OutcomeRejected", got, err)
	}
}
