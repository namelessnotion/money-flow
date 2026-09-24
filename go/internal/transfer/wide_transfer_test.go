package transfer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// incidentLegCount is the security_draw that halted the orchestrator on
// 2026-09-23: a security_escrow Wallet holding one Token per Subscription,
// 276 of them, drawn in full to the Borrower — one Transfer, 276 legs, more
// than one TigerBeetle batch can carry (go/docs/adr/0008).
const incidentLegCount = 276

const perTokenMinorUnits = 100

// wideTransferFixture opens a source Wallet holding n funded Tokens (minted
// in order, so FIFO selection takes them in that order) and an empty
// destination Wallet.
type wideTransferFixture struct {
	store    eventstore.Store
	lc       *ledger.FakeClient
	from, to string
	tokenIDs []string
}

func newWideTransferFixture(t *testing.T, n int) wideTransferFixture {
	t.Helper()
	f := wideTransferFixture{
		store: eventstore.NewMemoryStore(), lc: ledger.NewFakeClient(),
		from: testutil.ID("escrow"), to: testutil.ID("borrower"),
	}
	openWallet(t, f.store, f.from, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, f.store, f.to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	for i := range n {
		tokenID := testutil.ID(fmt.Sprintf("subscription-token-%d", i))
		mintToken(t, f.store, f.lc, f.from, tokenID, usd(perTokenMinorUnits))
		fundToken(t, f.lc, tokenID, perTokenMinorUnits)
		f.tokenIDs = append(f.tokenIDs, tokenID)
	}
	return f
}

func (f wideTransferFixture) total() uint64 {
	return uint64(len(f.tokenIDs)) * perTokenMinorUnits
}

// balances returns every source Token's balance plus the destination's.
func (f wideTransferFixture) balances(t *testing.T, transferID string) (sources []ledger.Balance, dest ledger.Balance) {
	t.Helper()
	ctx := context.Background()
	events, err := f.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	legs, err := preparedLegs(events, transferID)
	if err != nil {
		t.Fatalf("preparedLegs() error = %v", err)
	}
	all, err := f.lc.Balances(ctx, append(append([]string{}, f.tokenIDs...), legs[0].GetDestTokenId()))
	if err != nil {
		t.Fatalf("Balances() error = %v", err)
	}
	for _, id := range f.tokenIDs {
		sources = append(sources, all[id])
	}
	return sources, all[legs[0].GetDestTokenId()]
}

func (f wideTransferFixture) state(t *testing.T, transferID string) transferState {
	t.Helper()
	events, err := f.store.Load(context.Background(), AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return currentState(events)
}

func (f wideTransferFixture) prepare(t *testing.T, server *Server, transferID string, stage bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
		Id: transferID, FromWalletId: f.from, ToWalletId: f.to, Amount: usd(f.total()), Stage: stage,
	}); err != nil {
		t.Fatalf("seed TransferRequestAccepted: %v", err)
	}
	if err := server.prepare(ctx, transferID); err != nil {
		t.Fatalf("prepare(): %v", err)
	}
}

func TestCommit_TransferWiderThanOneLedgerBatchCommits(t *testing.T) {
	t.Parallel()
	f := newWideTransferFixture(t, incidentLegCount)
	server := NewServer(f.store, f.lc, nil, nil)
	transferID := testutil.ID("security-draw")

	if _, err := requestAndRun(t, server, context.Background(), transferRequest(transferID, f.from, f.to, usd(f.total()), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	if got := f.state(t, transferID); got != stateCommitted {
		t.Fatalf("state = %v, want committed", got)
	}
	sources, dest := f.balances(t, transferID)
	if want := (ledger.Balance{Currency: "USD", CreditsPosted: f.total()}); dest != want {
		t.Errorf("destination = %+v, want %+v", dest, want)
	}
	for i, b := range sources {
		if b.DebitsPosted != perTokenMinorUnits || b.DebitsPending != 0 {
			t.Fatalf("source %d = %+v, want %d posted and no reservation left", i, b, perTokenMinorUnits)
		}
	}
}

func TestStagedTransferWiderThanOneLedgerBatch_ReservesThenPostsEveryLeg(t *testing.T) {
	t.Parallel()
	f := newWideTransferFixture(t, incidentLegCount)
	server := NewServer(f.store, f.lc, nil, nil)
	transferID := testutil.ID("staged-security-draw")
	ctx := context.Background()

	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, f.from, f.to, usd(f.total()), true)); err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}
	if got := f.state(t, transferID); got != stateStaged {
		t.Fatalf("state = %v, want staged", got)
	}
	if _, dest := f.balances(t, transferID); dest.CreditsPending != f.total() || dest.CreditsPosted != 0 {
		t.Fatalf("destination after stage = %+v, want %d reserved and nothing posted", dest, f.total())
	}

	if _, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: transferID}); err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if _, err := server.PostPendingTransfer(ctx, &pb.PostPendingTransferRequest{Id: transferID}); err != nil {
		t.Fatalf("PostPendingTransfer() error = %v", err)
	}
	if got := f.state(t, transferID); got != stateCommitted {
		t.Fatalf("state = %v, want committed", got)
	}
	if _, dest := f.balances(t, transferID); dest != (ledger.Balance{Currency: "USD", CreditsPosted: f.total()}) {
		t.Fatalf("destination after post = %+v, want %d posted and nothing reserved", dest, f.total())
	}
}

func TestCancelStaged_WiderThanOneLedgerBatch_ReleasesEveryLeg(t *testing.T) {
	t.Parallel()
	f := newWideTransferFixture(t, incidentLegCount)
	server := NewServer(f.store, f.lc, nil, nil)
	transferID := testutil.ID("cancelled-security-draw")
	ctx := context.Background()

	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, f.from, f.to, usd(f.total()), true)); err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}
	if _, err := server.CancelStagedTransfer(ctx, &pb.CancelStagedTransferRequest{Id: transferID, Reason: "draw withdrawn"}); err != nil {
		t.Fatalf("CancelStagedTransfer() error = %v", err)
	}
	if got := f.state(t, transferID); got != stateCancelled {
		t.Fatalf("state = %v, want cancelled", got)
	}
	sources, dest := f.balances(t, transferID)
	if dest != (ledger.Balance{Currency: "USD"}) {
		t.Errorf("destination = %+v, want untouched", dest)
	}
	for i, b := range sources {
		if b.DebitsPending != 0 || b.DebitsPosted != 0 {
			t.Fatalf("source %d = %+v, want its reservation released", i, b)
		}
	}
}

// refusingClient refuses, with ExceedsCredits, every transfer of kind whose
// debit account is refuseDebit — the way TigerBeetle refuses one leg — and
// fails its whole linked chain with it, as TigerBeetle would.
type refusingClient struct {
	*ledger.FakeClient
	kind        ledger.TransferKind
	refuseDebit string
}

func (c *refusingClient) CreateTransfers(ctx context.Context, transfers []ledger.Transfer) ([]ledger.TransferResult, error) {
	for _, tr := range transfers {
		if tr.Kind == c.kind && tr.DebitAccountID == c.refuseDebit {
			results := make([]ledger.TransferResult, len(transfers))
			for i := range transfers {
				results[i] = ledger.TransferResult{Index: i, Result: ledger.TransferResultLinkedEventFailed}
			}
			return results, nil
		}
	}
	return c.FakeClient.CreateTransfers(ctx, transfers)
}

// A reservation refused in a later chain, after an earlier chain reserved,
// fails the Transfer like any other refused leg: nothing has been posted, so
// Failed is the truth. The earlier chain's reservation is left to its
// timeout (go/docs/adr/0008 says why it is not voided here).
func TestCommit_WideReservationRefusedPartwayFailsTheTransferAndPostsNothing(t *testing.T) {
	t.Parallel()
	f := newWideTransferFixture(t, incidentLegCount)
	lastToken := f.tokenIDs[len(f.tokenIDs)-1]
	server := NewServer(f.store, &refusingClient{FakeClient: f.lc, kind: ledger.TransferKindPending, refuseDebit: lastToken}, nil, nil)
	transferID := testutil.ID("draw-refused-reserve")
	f.prepare(t, server, transferID, false)

	if err := server.commit(context.Background(), transferID); err != nil {
		t.Fatalf("commit() error = %v, want nil: a refused reservation fails the Transfer", err)
	}
	if got := f.state(t, transferID); got != stateFailed {
		t.Fatalf("state = %v, want failed", got)
	}
	sources, dest := f.balances(t, transferID)
	if dest.CreditsPosted != 0 {
		t.Errorf("destination = %+v, want nothing posted", dest)
	}
	for i, b := range sources {
		if b.DebitsPosted != 0 {
			t.Fatalf("source %d = %+v, want nothing posted", i, b)
		}
	}
}

// Once one chain has posted, a later chain's refusal cannot be expressed as
// Failed: money has moved. The step errors instead, so the orchestrator
// halts on it and a person reconciles (go/docs/adr/0003, 0008).
func TestCommit_PostRefusedAfterAnEarlierChainPostedHaltsRatherThanFailing(t *testing.T) {
	t.Parallel()
	f := newWideTransferFixture(t, incidentLegCount)
	lastToken := f.tokenIDs[len(f.tokenIDs)-1]
	server := NewServer(f.store, &refusingClient{FakeClient: f.lc, kind: ledger.TransferKindPostPending, refuseDebit: lastToken}, nil, nil)
	transferID := testutil.ID("draw-refused-post")
	f.prepare(t, server, transferID, false)

	if err := server.commit(context.Background(), transferID); err == nil {
		t.Fatalf("commit() error = nil, want an error: part of the Transfer has posted")
	}
	if got := f.state(t, transferID); got != statePrepared {
		t.Fatalf("state = %v, want still prepared (neither Failed nor Committed is true)", got)
	}
}

// A refused post in the first chain means nothing posted, so the Transfer
// still fails cleanly, exactly as a narrow Transfer's refused post does.
func TestCommit_PostRefusedInTheFirstChainFailsTheTransfer(t *testing.T) {
	t.Parallel()
	f := newWideTransferFixture(t, incidentLegCount)
	server := NewServer(f.store, &refusingClient{FakeClient: f.lc, kind: ledger.TransferKindPostPending, refuseDebit: f.tokenIDs[0]}, nil, nil)
	transferID := testutil.ID("draw-refused-first-post")
	f.prepare(t, server, transferID, false)

	if err := server.commit(context.Background(), transferID); err != nil {
		t.Fatalf("commit() error = %v, want nil", err)
	}
	if got := f.state(t, transferID); got != stateFailed {
		t.Fatalf("state = %v, want failed", got)
	}
	if _, dest := f.balances(t, transferID); dest.CreditsPosted != 0 {
		t.Fatalf("destination = %+v, want nothing posted", dest)
	}
}

// invalidRequestClient refuses every CreateTransfers call as a whole, the
// way the real client reports a request TigerBeetle can never accept.
type invalidRequestClient struct {
	ledger.Client
	mu    sync.Mutex
	calls int
}

func (c *invalidRequestClient) CreateTransfers(context.Context, []ledger.Transfer) ([]ledger.TransferResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return nil, fmt.Errorf("ledger: CreateTransfers: %w: too much data", ledger.ErrInvalidRequest)
}

// An invalid request is deterministic: every retry fails the same way, so
// returning it as an error only halts the orchestrator's partition on a
// message no retry gets past (the 2026-09-23 incident). It fails the
// Transfer instead, which its Transaction compensates.
func TestSaga_InvalidLedgerRequestFailsTheTransferInsteadOfErroring(t *testing.T) {
	t.Parallel()
	for _, stage := range []bool{false, true} {
		t.Run(fmt.Sprintf("stage=%v", stage), func(t *testing.T) {
			t.Parallel()
			store := eventstore.NewMemoryStore()
			lc := ledger.NewFakeClient()
			openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
			openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
			mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
			fundToken(t, lc, testutil.ID("t1"), 1000)

			server := NewServer(store, &invalidRequestClient{Client: lc}, nil, nil)
			transferID := testutil.ID(fmt.Sprintf("xfer-invalid-%v", stage))
			ctx := context.Background()
			if _, err := server.RequestTransfer(ctx, transferRequest(transferID, testutil.ID("w1"), testutil.ID("w2"), usd(400), stage)); err != nil {
				t.Fatalf("RequestTransfer() error = %v", err)
			}
			if err := server.Resume(ctx, transferID); err != nil {
				t.Fatalf("Resume() error = %v, want nil: the Transfer should fail, not the orchestrator", err)
			}

			events, err := store.Load(ctx, AggregateType, transferID)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if currentState(events) != stateFailed {
				t.Fatalf("state = %v, want failed; events = %v", currentState(events), eventTypes(events))
			}
		})
	}
}

// A transport failure is not a verdict on the request and must still reach
// the orchestrator's retry-then-halt policy rather than fail the Transfer.
func TestSaga_TransientLedgerErrorStillErrors(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, &transientErrorClient{Client: lc}, nil, nil)
	transferID := testutil.ID("xfer-transient")
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, store, transferID, testutil.ID("w1"), testutil.ID("w2"), usd(400))

	if err := server.stage(ctx, transferID); err == nil {
		t.Fatalf("stage() error = nil, want the transport error surfaced for retry")
	}
	if got := currentStateOf(t, store, transferID); got != statePrepared {
		t.Fatalf("state = %v, want still prepared", got)
	}
}

type transientErrorClient struct{ ledger.Client }

var errTransport = errors.New("ledger: CreateTransfers: client evicted")

func (c *transientErrorClient) CreateTransfers(context.Context, []ledger.Transfer) ([]ledger.TransferResult, error) {
	return nil, errTransport
}

func currentStateOf(t *testing.T, store eventstore.Store, transferID string) transferState {
	t.Helper()
	events, err := store.Load(context.Background(), AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return currentState(events)
}
