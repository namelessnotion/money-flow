package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

func lastEventType(t *testing.T, store eventstore.Store, transferID string) string {
	t.Helper()
	events, err := store.Load(context.Background(), AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("stream for %q is empty", transferID)
	}
	return events[len(events)-1].EventType
}

func TestRequestTransfer_RejectedRequestIsIdempotent(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(100))
	fundToken(t, lc, testutil.ID("t1"), 100)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	req := transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false)

	first, err := requestAndRun(t, server, ctx, req)
	if err != nil {
		t.Fatalf("first RequestTransfer() error = %v", err)
	}
	second, err := requestAndRun(t, server, ctx, req)
	if err != nil {
		t.Fatalf("second RequestTransfer() error = %v", err)
	}
	if second.GetTransferRequestRejected() == nil {
		t.Fatalf("result = %v, want TransferRequestRejected on replay", second.GetResult())
	}
	if first.GetTransferRequestRejected().GetReason() != second.GetTransferRequestRejected().GetReason() {
		t.Errorf("replay reason = %q, want the recorded reason %q", second.GetTransferRequestRejected().GetReason(), first.GetTransferRequestRejected().GetReason())
	}

	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Errorf("stream holds %d events, want 1 (no duplicate rejection on replay)", len(events))
	}
}

func TestRequestReversal_RejectionIsRecorded(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()

	resp, err := reverseAndRun(t, server, ctx, &pb.RequestReversalRequest{
		Id: testutil.ID("rev1"), TransferId: testutil.ID("never-existed"), Reason: "test",
	})
	if err != nil {
		t.Fatalf("RequestReversal() error = %v", err)
	}
	if resp.GetReversalRequestRejected() == nil {
		t.Fatalf("result = %v, want ReversalRequestRejected", resp.GetResult())
	}

	if got := lastEventType(t, store, testutil.ID("rev1")); got != "transfer.v1.ReversalRequestRejected" {
		t.Errorf("recorded event type = %q, want ReversalRequestRejected", got)
	}
}

func TestConfirmStagedTransfer_RejectionIsRecordedOntoExistingStream(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	// Not staged — commits immediately, ending up Committed.
	if _, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	before := len(mustEvents(t, store, testutil.ID("xfer1")))

	resp, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: testutil.ID("xfer1")})
	if err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if resp.GetConfirmStagedTransferRejected() == nil {
		t.Fatalf("result = %v, want ConfirmStagedTransferRejected", resp.GetResult())
	}

	after := mustEvents(t, store, testutil.ID("xfer1"))
	if len(after) != before+1 {
		t.Fatalf("stream holds %d events, want %d (the rejection appended onto the existing history)", len(after), before+1)
	}
	if after[len(after)-1].EventType != "transfer.v1.ConfirmStagedTransferRejected" {
		t.Errorf("last event type = %q, want ConfirmStagedTransferRejected", after[len(after)-1].EventType)
	}
	// The rejection must not disturb what state the Transfer is actually in.
	if currentState(after) != stateCommitted {
		t.Errorf("state after rejection = %v, want still committed", currentState(after))
	}
}

func TestCancelStagedTransfer_RejectionIsRecordedOntoExistingStream(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	if _, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	resp, err := server.CancelStagedTransfer(ctx, &pb.CancelStagedTransferRequest{Id: testutil.ID("xfer1"), Reason: "too late"})
	if err != nil {
		t.Fatalf("CancelStagedTransfer() error = %v", err)
	}
	if resp.GetCancelStagedTransferRejected() == nil {
		t.Fatalf("result = %v, want CancelStagedTransferRejected", resp.GetResult())
	}
	if got := lastEventType(t, store, testutil.ID("xfer1")); got != "transfer.v1.CancelStagedTransferRejected" {
		t.Errorf("last event type = %q, want CancelStagedTransferRejected", got)
	}
}

func TestPostPendingTransfer_RejectionIsRecordedOntoExistingStream(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	ctx := context.Background()

	// A bare Accepted transfer — not Pending, not Committed — is neither of
	// PostPendingTransfer's idempotent states, so it must be rejected.
	transferID := testutil.ID("xfer1")
	if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
		Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400),
	}); err != nil {
		t.Fatalf("seed accepted: %v", err)
	}

	server := NewServer(store, lc, nil, nil)
	resp, err := server.PostPendingTransfer(ctx, &pb.PostPendingTransferRequest{Id: transferID})
	if err != nil {
		t.Fatalf("PostPendingTransfer() error = %v", err)
	}
	if resp.GetPostPendingTransferRejected() == nil {
		t.Fatalf("result = %v, want PostPendingTransferRejected", resp.GetResult())
	}
	if got := lastEventType(t, store, testutil.ID("xfer1")); got != "transfer.v1.PostPendingTransferRejected" {
		t.Errorf("last event type = %q, want PostPendingTransferRejected", got)
	}
}

func TestRejections_NotFoundStaysUnrecorded(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	id := testutil.ID("never-existed")

	if resp, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: id}); err != nil || resp.GetConfirmStagedTransferRejected() == nil {
		t.Fatalf("ConfirmStagedTransfer() = (%v, %v), want a rejection", resp, err)
	}
	events, err := store.Load(ctx, AggregateType, id)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 0 {
		t.Errorf("stream holds %d events, want 0 — an id that was never requested has no aggregate to record onto", len(events))
	}
}

func mustEvents(t *testing.T, store eventstore.Store, transferID string) []eventstore.Event {
	t.Helper()
	events, err := store.Load(context.Background(), AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return events
}

// conflictNTimesStore forces the first n calls to Append to lose an
// optimistic-concurrency race, running onConflict before the very first one
// so a concurrent winner is already in the log by the time a retry looks.
// AppendAtomic and Load pass through to the real store untouched.
type conflictNTimesStore struct {
	eventstore.Store
	n          int
	onConflict func()
}

func (s *conflictNTimesStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	if s.n > 0 {
		s.n--
		if s.onConflict != nil {
			cb := s.onConflict
			s.onConflict = nil
			cb()
		}
		return eventstore.ErrConcurrencyConflict
	}
	return s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
}

func TestRequestTransfer_ConvergesPastATransientConflict(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(100))
	fundToken(t, lc, testutil.ID("t1"), 100)

	// Simulates a concurrent, identical RequestTransfer call reaching the
	// same rejection and winning the race to record it first.
	wrapped := &conflictNTimesStore{Store: store, n: 1, onConflict: func() {
		if err := store.Append(context.Background(), AggregateType, testutil.ID("xfer1"), 0,
			&pb.TransferRequestRejected{Id: testutil.ID("xfer1"), Reason: "a concurrent caller's rejection"},
		); err != nil {
			t.Errorf("seeding concurrent winner: %v", err)
		}
	}}

	server := NewServer(wrapped, lc, nil, nil)
	resp, err := server.RequestTransfer(context.Background(),
		transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false))
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v, want it to converge on the concurrent winner instead", err)
	}
	if resp.GetTransferRequestRejected().GetReason() != "a concurrent caller's rejection" {
		t.Errorf("result = %v, want the concurrent winner's rejection", resp.GetResult())
	}

	events, err := store.Load(context.Background(), AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Errorf("stream holds %d events, want 1 (only the winner's, no duplicate)", len(events))
	}
}

func TestRequestTransfer_ReturnsAbortedAfterExhaustingRetries(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(100))
	fundToken(t, lc, testutil.ID("t1"), 100)

	// Always conflicts, never lets a winner land — a persistent race this
	// call can never resolve on its own.
	wrapped := &conflictNTimesStore{Store: store, n: maxConcurrencyAttempts + 1}

	server := NewServer(wrapped, lc, nil, nil)
	_, err := server.RequestTransfer(context.Background(),
		transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false))
	if err == nil {
		t.Fatal("RequestTransfer() error = nil, want a retryable error after exhausting every attempt")
	}
	var twerr twirp.Error
	if !errors.As(err, &twerr) {
		t.Fatalf("error = %T, want twirp.Error", err)
	}
	if twerr.Code() != twirp.Aborted {
		t.Errorf("error code = %q, want %q (a client should know to retry)", twerr.Code(), twirp.Aborted)
	}
}
