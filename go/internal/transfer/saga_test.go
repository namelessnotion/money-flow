package transfer

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/token"
)

// requestAndRun does what RequestTransfer alone used to do: record the
// acceptance, then run the saga. Since the async cutover the RPC only records,
// and cmd/orchestrator runs the saga from the event that acceptance publishes —
// so a test that wants a Transfer to actually move has to drive it, and these
// call sites say so rather than the RPC quietly doing both.
//
// It resumes only an accepted Transfer: a rejection opens and closes the stream
// in one event and has no saga to run.
func requestAndRun(t *testing.T, s *Server, ctx context.Context, req *pb.RequestTransferRequest) (*pb.RequestTransferResponse, error) {
	t.Helper()
	resp, err := s.RequestTransfer(ctx, req)
	if err != nil || resp.GetTransferRequestAccepted() == nil {
		return resp, err
	}
	if err := s.Resume(ctx, req.GetId()); err != nil {
		t.Fatalf("requestAndRun(%s): resume: %v", req.GetId(), err)
	}
	return resp, nil
}

// reverseAndRun is requestAndRun for a Reversal, which is a Transfer too.
func reverseAndRun(t *testing.T, s *Server, ctx context.Context, req *pb.RequestReversalRequest) (*pb.RequestReversalResponse, error) {
	t.Helper()
	resp, err := s.RequestReversal(ctx, req)
	if err != nil || resp.GetReversalRequestAccepted() == nil {
		return resp, err
	}
	if err := s.Resume(ctx, req.GetId()); err != nil {
		t.Fatalf("reverseAndRun(%s): resume: %v", req.GetId(), err)
	}
	return resp, nil
}

func transferRequest(id, fromWallet, toWallet string, amount *sharedpb.Money, stage bool) *pb.RequestTransferRequest {
	return &pb.RequestTransferRequest{Id: id, FromWalletId: fromWallet, ToWalletId: toWallet, Amount: amount, Stage: stage}
}

// lastBalanceRecorded returns the newest TokenBalanceRecorded on tokenID's
// stream — what the read side shows for it — or nil if none.
func lastBalanceRecorded(t *testing.T, store eventstore.Store, tokenID string) *tokenpb.TokenBalanceRecorded {
	t.Helper()
	events, err := store.Load(context.Background(), token.AggregateType, tokenID)
	if err != nil {
		t.Fatalf("Load(token) error = %v", err)
	}
	var last *tokenpb.TokenBalanceRecorded
	for _, e := range events {
		msg, err := e.Decode()
		if err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		if r, ok := msg.(*tokenpb.TokenBalanceRecorded); ok {
			last = r
		}
	}
	return last
}

// assertBalanceRecorded checks the balance the read side would show for
// tokenID.
func assertBalanceRecorded(t *testing.T, store eventstore.Store, tokenID string, posted int64, outgoing, incoming uint64) {
	t.Helper()
	got := lastBalanceRecorded(t, store, tokenID)
	if got.GetPostedMinorUnits() != posted || got.GetPendingOutgoingMinorUnits() != outgoing || got.GetPendingIncomingMinorUnits() != incoming {
		t.Errorf("token %s recorded %v, want posted=%d outgoing=%d incoming=%d", tokenID, got, posted, outgoing, incoming)
	}
}

func TestRequestTransfer_OneToOne(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	resp, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false))
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("result = %v, want Accepted", resp.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateCommitted {
		t.Fatalf("state = %v, want committed; events = %v", currentState(events), eventTypes(events))
	}

	committed, err := decodedCommitted(events)
	if err != nil {
		t.Fatalf("decodedCommitted() error = %v", err)
	}
	if len(committed.GetDestinations()) != 1 || committed.GetDestinations()[0].GetAmount().GetMinorUnits() != 400 {
		t.Fatalf("destinations = %v, want one of 400", committed.GetDestinations())
	}
	destTokenID := committed.GetDestinations()[0].GetToTokenId()

	if balance, _, _ := ledger.AccountBalance(ctx, lc, destTokenID); balance != 400 {
		t.Errorf("dest balance = %d, want 400", balance)
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 600 {
		t.Errorf("source balance = %d, want 600 (1000 - 400)", balance)
	}
	assertBalanceRecorded(t, store, destTokenID, 400, 0, 0)
	assertBalanceRecorded(t, store, testutil.ID("t1"), 600, 0, 0)
}

func TestRequestTransfer_ManyToOneFIFO(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(300))
	fundToken(t, lc, testutil.ID("t1"), 300)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t2"), usd(300))
	fundToken(t, lc, testutil.ID("t2"), 300)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	resp, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false))
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("result = %v, want Accepted", resp.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateCommitted {
		t.Fatalf("state = %v, want committed", currentState(events))
	}
	legs, err := preparedLegs(events, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("preparedLegs() error = %v", err)
	}
	if len(legs) != 2 {
		t.Fatalf("legs = %v, want 2 (many-to-one)", legs)
	}
	if legs[0].GetSourceTokenId() != testutil.ID("t1") || legs[0].GetAmount().GetMinorUnits() != 300 {
		t.Errorf("legs[0] = %+v, want t1 drained fully (300, FIFO oldest first)", legs[0])
	}
	if legs[1].GetSourceTokenId() != testutil.ID("t2") || legs[1].GetAmount().GetMinorUnits() != 100 {
		t.Errorf("legs[1] = %+v, want t2 for the remainder (100)", legs[1])
	}

	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 0 {
		t.Errorf("t1 balance = %d, want 0 (fully drained)", balance)
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t2")); balance != 200 {
		t.Errorf("t2 balance = %d, want 200 (300 - 100)", balance)
	}
}

func TestRequestTransfer_InsufficientBalanceRejects(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(100))
	fundToken(t, lc, testutil.ID("t1"), 100)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	resp, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false))
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if resp.GetTransferRequestRejected() == nil {
		t.Fatalf("result = %v, want TransferRequestRejected", resp.GetResult())
	}

	// The rejection is itself a fact about this id's history, not just a
	// transient response — it must be durably recorded, the same as an
	// acceptance would be.
	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("stream holds %d events, want 1 (the rejection itself)", len(events))
	}
	if events[0].EventType != "transfer.v1.TransferRequestRejected" {
		t.Errorf("recorded event type = %q, want TransferRequestRejected", events[0].EventType)
	}
}

func TestRequestTransfer_IsIdempotent(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	req := transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false)
	if _, err := requestAndRun(t, server, ctx, req); err != nil {
		t.Fatalf("first RequestTransfer() error = %v", err)
	}
	if _, err := requestAndRun(t, server, ctx, req); err != nil {
		t.Fatalf("second RequestTransfer() error = %v", err)
	}

	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 600 {
		t.Errorf("source balance = %d, want 600 (no double-debit on replay)", balance)
	}
}

func TestStagedTransfer_HappyPath(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	resp, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), true))
	if err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}
	if resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("result = %v, want Accepted", resp.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateStaged {
		t.Fatalf("state = %v, want staged (saga must stop there)", currentState(events))
	}
	legs, err := preparedLegs(events, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("preparedLegs() error = %v", err)
	}
	destTokenID := legs[0].GetDestTokenId()
	if balance, _, _ := ledger.AccountBalance(ctx, lc, destTokenID); balance != 0 {
		t.Errorf("dest posted balance = %d, want 0 (still only reserved, not posted)", balance)
	}
	assertBalanceRecorded(t, store, destTokenID, 0, 0, 400)
	assertBalanceRecorded(t, store, testutil.ID("t1"), 1000, 400, 0)

	confirmResp, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: testutil.ID("xfer1")})
	if err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if confirmResp.GetTransferPending() == nil {
		t.Fatalf("result = %v, want TransferPending", confirmResp.GetResult())
	}
	events, err = store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != statePending {
		t.Fatalf("state = %v, want pending", currentState(events))
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, destTokenID); balance != 0 {
		t.Errorf("dest posted balance after confirm = %d, want still 0 (no tigerbeetle call happens here)", balance)
	}

	postResp, err := server.PostPendingTransfer(ctx, &pb.PostPendingTransferRequest{Id: testutil.ID("xfer1")})
	if err != nil {
		t.Fatalf("PostPendingTransfer() error = %v", err)
	}
	if postResp.GetTransferCommitted() == nil {
		t.Fatalf("result = %v, want TransferCommitted", postResp.GetResult())
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, destTokenID); balance != 400 {
		t.Errorf("dest posted balance after post = %d, want 400", balance)
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 600 {
		t.Errorf("source balance = %d, want 600", balance)
	}
	assertBalanceRecorded(t, store, destTokenID, 400, 0, 0)
	assertBalanceRecorded(t, store, testutil.ID("t1"), 600, 0, 0)
}

func TestCancelStagedTransfer_FromStaged(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	if _, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), true)); err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}

	resp, err := server.CancelStagedTransfer(ctx, &pb.CancelStagedTransferRequest{Id: testutil.ID("xfer1"), Reason: "provisioning failed"})
	if err != nil {
		t.Fatalf("CancelStagedTransfer() error = %v", err)
	}
	if resp.GetTransferCancelled() == nil {
		t.Fatalf("result = %v, want TransferCancelled", resp.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateCancelled {
		t.Fatalf("state = %v, want cancelled", currentState(events))
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 1000 {
		t.Errorf("source balance = %d, want 1000 (reservation released, nothing ever posted)", balance)
	}
	assertBalanceRecorded(t, store, testutil.ID("t1"), 1000, 0, 0)

	// Idempotent retry converges.
	if _, err := server.CancelStagedTransfer(ctx, &pb.CancelStagedTransferRequest{Id: testutil.ID("xfer1"), Reason: "again"}); err != nil {
		t.Fatalf("CancelStagedTransfer() retry error = %v", err)
	}
}

func TestCancelStagedTransfer_FromPending(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	if _, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), true)); err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}
	if _, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: testutil.ID("xfer1")}); err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}

	resp, err := server.CancelStagedTransfer(ctx, &pb.CancelStagedTransferRequest{Id: testutil.ID("xfer1"), Reason: "provider returned it"})
	if err != nil {
		t.Fatalf("CancelStagedTransfer() error = %v", err)
	}
	if resp.GetTransferCancelled() == nil {
		t.Fatalf("result = %v, want TransferCancelled", resp.GetResult())
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 1000 {
		t.Errorf("source balance = %d, want 1000 (reservation released)", balance)
	}
}

func TestRequestReversal_OfCommittedTransfer(t *testing.T) {
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
	events, err := store.Load(ctx, AggregateType, testutil.ID("xfer1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	committed, err := decodedCommitted(events)
	if err != nil {
		t.Fatalf("decodedCommitted() error = %v", err)
	}
	destTokenID := committed.GetDestinations()[0].GetToTokenId()

	resp, err := reverseAndRun(t, server, ctx, &pb.RequestReversalRequest{
		Id: testutil.ID("rev1"), TransferId: testutil.ID("xfer1"), Reason: "customer disputed",
	})
	if err != nil {
		t.Fatalf("RequestReversal() error = %v", err)
	}
	if resp.GetReversalRequestAccepted() == nil {
		t.Fatalf("result = %v, want Accepted", resp.GetResult())
	}

	revEvents, err := store.Load(ctx, AggregateType, testutil.ID("rev1"))
	if err != nil {
		t.Fatalf("Load(reversal) error = %v", err)
	}
	if currentState(revEvents) != stateCommitted {
		t.Fatalf("reversal state = %v, want committed; events = %v", currentState(revEvents), eventTypes(revEvents))
	}
	revLegs, err := preparedLegs(revEvents, testutil.ID("rev1"))
	if err != nil {
		t.Fatalf("preparedLegs(reversal) error = %v", err)
	}
	if len(revLegs) != 1 || revLegs[0].GetSourceTokenId() != destTokenID || revLegs[0].GetDestTokenId() != testutil.ID("t1") {
		t.Fatalf("reversal legs = %+v, want one leg from %s back to t1", revLegs, destTokenID)
	}

	if balance, _, _ := ledger.AccountBalance(ctx, lc, destTokenID); balance != 0 {
		t.Errorf("original destination balance after reversal = %d, want 0", balance)
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 1000 {
		t.Errorf("original source balance after reversal = %d, want 1000 (fully restored)", balance)
	}
}

func TestReversal_OfManyToOneProducesOneToMany(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(300))
	fundToken(t, lc, testutil.ID("t1"), 300)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t2"), usd(300))
	fundToken(t, lc, testutil.ID("t2"), 300)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	if _, err := requestAndRun(t, server, ctx, transferRequest(testutil.ID("xfer1"), testutil.ID("w1"), testutil.ID("w2"), usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	if _, err := reverseAndRun(t, server, ctx, &pb.RequestReversalRequest{Id: testutil.ID("rev1"), TransferId: testutil.ID("xfer1"), Reason: "reverse it all"}); err != nil {
		t.Fatalf("RequestReversal() error = %v", err)
	}

	revEvents, err := store.Load(ctx, AggregateType, testutil.ID("rev1"))
	if err != nil {
		t.Fatalf("Load(reversal) error = %v", err)
	}
	revLegs, err := preparedLegs(revEvents, testutil.ID("rev1"))
	if err != nil {
		t.Fatalf("preparedLegs(reversal) error = %v", err)
	}
	if len(revLegs) != 2 {
		t.Fatalf("reversal legs = %+v, want 2 (one-to-many, mirroring the original many-to-one)", revLegs)
	}

	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 300 {
		t.Errorf("t1 balance = %d, want 300 (fully restored)", balance)
	}
	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t2")); balance != 300 {
		t.Errorf("t2 balance = %d, want 300 (fully restored)", balance)
	}
}

func TestCancelAcceptedTransfer_FromAccepted(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	ctx := context.Background()

	// Seed a bare Accepted event directly, bypassing RequestTransfer (which
	// would run the saga straight through to Committed).
	transferID := testutil.ID("xfer1")
	if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
		Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400),
	}); err != nil {
		t.Fatalf("seed accepted: %v", err)
	}

	server := NewServer(store, lc, nil, nil)
	resp, err := server.CancelAcceptedTransfer(ctx, &pb.CancelAcceptedTransferRequest{Id: transferID, Reason: "changed my mind"})
	if err != nil {
		t.Fatalf("CancelAcceptedTransfer() error = %v", err)
	}
	if resp.GetAcceptedTransferCancelled() == nil {
		t.Fatalf("result = %v, want AcceptedTransferCancelled", resp.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != stateCancelled {
		t.Fatalf("state = %v, want cancelled", currentState(events))
	}
}

func eventTypes(events []eventstore.Event) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.EventType
	}
	return types
}
