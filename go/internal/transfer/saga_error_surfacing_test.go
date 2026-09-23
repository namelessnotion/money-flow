package transfer

import (
	"context"
	"errors"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// failingAppendAtomicStore lets Append and Load through to the real store
// (so RequestTransfer's own accept step succeeds normally) but makes every
// AppendAtomic call fail with a plain, non-domain error — simulating the
// store becoming unavailable partway through prepare(), which is the first
// thing the saga does after accept.
type failingAppendAtomicStore struct {
	eventstore.Store
	err error
}

func (s *failingAppendAtomicStore) AppendAtomic(context.Context, ...eventstore.StreamWrite) error {
	return s.err
}

// Two halves of one guarantee, and the shape of it changed with the cutover.
//
// Accepting is durable on its own: RequestTransfer records the acceptance and
// answers, so a saga that cannot run afterwards does not cost the caller its
// answer or the Transfer its existence.
//
// And the failure is no longer swallowed. It used to be logged and dropped,
// because the only thing that could have seen it was an RPC whose response was
// already decided. Now the driver is the orchestrator, and it needs the error
// back: whether a trigger is retried or the consumer halts on it is its
// decision (go/docs/adr/0003), and it cannot make that decision about an error
// it never receives.
//
// A TigerBeetle rejection is deliberately not this case — those become a
// durable TransferFailed event and Resume returns nil. This is the genuinely
// internal kind.
func TestRequestTransfer_AcceptsDurablyAndSurfacesTheSagaFailureToItsDriver(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	boom := errors.New("boom: store unavailable")
	failing := &failingAppendAtomicStore{Store: store, err: boom}
	server := NewServer(failing, lc, nil, nil)
	ctx := context.Background()
	transferID := testutil.ID("xfer1")

	resp, err := server.RequestTransfer(ctx, transferRequest(transferID, testutil.ID("w1"), testutil.ID("w2"), usd(400), false))
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v, want the Accepted response — accepting is its whole job", err)
	}
	if resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("result = %v, want Accepted", resp.GetResult())
	}

	if err := server.Resume(ctx, transferID); !errors.Is(err, boom) {
		t.Fatalf("Resume() error = %v, want the store's own error back so the orchestrator can decide what to do with it", err)
	}

	// The acceptance is still the only thing on the stream: a saga that could
	// not run left no half-written state behind.
	events, loadErr := store.Load(ctx, AggregateType, transferID)
	if loadErr != nil {
		t.Fatalf("Load() error = %v", loadErr)
	}
	if currentState(events) != stateAccepted {
		t.Errorf("state = %v, want accepted", currentState(events))
	}
}
