package transfer

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// rejectingTransfersClient wraps a real ledger.Client but forces every
// CreateTransfers call to report a rejection instead of delegating,
// simulating TigerBeetle refusing a batch we submit — an internal
// invariant failure, per decision #13, distinct from an external-factor
// cancellation.
type rejectingTransfersClient struct {
	ledger.Client
}

func (r *rejectingTransfersClient) CreateTransfers(_ context.Context, transfers []ledger.Transfer) ([]ledger.TransferResult, error) {
	results := make([]ledger.TransferResult, len(transfers))
	for i := range transfers {
		results[i] = ledger.TransferResult{Index: i, Result: ledger.TransferResultExceedsCredits}
	}
	return results, nil
}

func TestCommit_TigerBeetleRejectionRoutesToFailedNotCancelled(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	// prepare() (minting) uses the real fake client; only the commit-time
	// CreateTransfers batch gets rejected.
	server := NewServer(store, &rejectingTransfersClient{Client: lc}, nil, nil)
	ctx := context.Background()
	if _, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateFailed {
		t.Fatalf("state = %v, want failed (not cancelled); events = %v", currentState(events), eventTypes(events))
	}
}

func TestStage_TigerBeetleRejectionRoutesToFailed(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, &rejectingTransfersClient{Client: lc}, nil, nil)
	ctx := context.Background()
	if _, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), true)); err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}

	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateFailed {
		t.Fatalf("state = %v, want failed (staging batch itself was rejected)", currentState(events))
	}
}

// TestStage_TigerBeetleRejectionReturnsNilNotAContradiction is
// TestStage_TigerBeetleRejectionRoutesToFailed's blind spot closed:
// RequestTransfer drives runSaga through logSagaError, which logs a saga
// failure rather than returning it, so the two RequestTransfer-based tests
// above can't see that stage()/commit() themselves used to return a
// contradiction error here — submitBatch returned onReject's result
// directly, and a *successful* compensate() (which legitimately returns nil
// after appending TransferFailed) was indistinguishable from "no rejection
// happened," so stage() fell through to recording TransferStaged on a
// Transfer compensate() had just marked Failed. The Transfer's own final state
// (Failed, appended durably by compensate() before the fall-through) stayed
// correct either way, which is exactly why this needed its own test rather
// than trusting the existing ones' state assertions.
func TestStage_TigerBeetleRejectionReturnsNilNotAContradiction(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	transferID := testutil.ID("xfer-stage-rejected")
	server := NewServer(store, &rejectingTransfersClient{Client: lc}, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, store, transferID, testutil.ID("w1"), testutil.ID("w2"), usd(400))

	if err := server.stage(ctx, transferID); err != nil {
		t.Fatalf("stage() error = %v, want nil: compensate() already recorded the rejection cleanly", err)
	}

	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateFailed {
		t.Fatalf("state = %v, want failed", currentState(events))
	}
}

// TestCommit_TigerBeetleRejectionReturnsNilNotAContradiction is
// the commit() twin of the stage() test above.
func TestCommit_TigerBeetleRejectionReturnsNilNotAContradiction(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	transferID := testutil.ID("xfer-commit-rejected")
	server := NewServer(store, &rejectingTransfersClient{Client: lc}, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, store, transferID, testutil.ID("w1"), testutil.ID("w2"), usd(400))

	if err := server.commit(ctx, transferID); err != nil {
		t.Fatalf("commit() error = %v, want nil: compensate() already recorded the rejection cleanly", err)
	}

	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateFailed {
		t.Fatalf("state = %v, want failed", currentState(events))
	}
}
