package transfer

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	walletpb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/token"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

func usd(minorUnits uint64) *sharedpb.Money {
	return &sharedpb.Money{MinorUnits: minorUnits, Currency: "USD"}
}

// mintToken opens (if needed) and mints a Token of the given capacity into
// walletID, returning the Token's id.
func mintToken(t *testing.T, store eventstore.Store, lc ledger.Client, walletID, tokenID string, capacity *sharedpb.Money) {
	t.Helper()
	ts := token.NewServer(store, lc)
	resp, err := ts.Mint(context.Background(), &tokenpb.MintRequest{Id: tokenID, WalletId: walletID, Capacity: capacity})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if resp.GetTokenMintRejected() != nil {
		t.Fatalf("mint token rejected: %v", resp.GetTokenMintRejected())
	}
}

// fundToken gives tokenID a posted TigerBeetle balance for tests that need
// selectSourceTokens to find real capacity, by transferring in from a
// throwaway external account.
func fundToken(t *testing.T, lc ledger.Client, tokenID string, minorUnits uint64) {
	t.Helper()
	ctx := context.Background()
	source := ledger.Account{ID: testutil.ID("external-funding-" + tokenID), Currency: "USD"}
	if _, err := lc.CreateAccounts(ctx, []ledger.Account{source}); err != nil {
		t.Fatalf("fundToken: create external account: %v", err)
	}
	xfer := ledger.Transfer{
		ID: testutil.ID("fund-" + tokenID), DebitAccountID: source.ID, CreditAccountID: tokenID,
		MinorUnits: minorUnits, Currency: "USD", Kind: ledger.TransferKindRegular,
	}
	results, err := lc.CreateTransfers(ctx, []ledger.Transfer{xfer})
	if err != nil {
		t.Fatalf("fundToken: transfer: %v", err)
	}
	if results[0].Result != ledger.TransferResultOK {
		t.Fatalf("fundToken: transfer result = %v, want OK", results[0].Result)
	}
}

func openWallet(t *testing.T, store eventstore.Store, walletID string, allows sharedpb.Allows) {
	t.Helper()
	if _, err := wallet.NewServer(store).Open(context.Background(), &walletpb.OpenRequest{
		Id: walletID, HolderId: testutil.ID("h1"), Name: "test wallet", Allows: allows,
	}); err != nil {
		t.Fatalf("open wallet: %v", err)
	}
}

func TestSelectSourceTokens_SingleTokenCovers(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	legs, rejection, err := selectSourceTokens(context.Background(), store, lc, testutil.ID("w1"), usd(400), "", nil)
	if err != nil {
		t.Fatalf("selectSourceTokens() error = %v", err)
	}
	if rejection != nil {
		t.Fatalf("rejection = %v, want none", rejection)
	}
	if len(legs) != 1 || legs[0].SourceTokenID != testutil.ID("t1") || legs[0].Amount.GetMinorUnits() != 400 {
		t.Fatalf("legs = %+v, want one leg of 400 from t1", legs)
	}
}

func TestSelectSourceTokens_ManyToOneFIFO(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(300))
	fundToken(t, lc, testutil.ID("t1"), 300)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t2"), usd(300))
	fundToken(t, lc, testutil.ID("t2"), 300)

	legs, rejection, err := selectSourceTokens(context.Background(), store, lc, testutil.ID("w1"), usd(400), "", nil)
	if err != nil {
		t.Fatalf("selectSourceTokens() error = %v", err)
	}
	if rejection != nil {
		t.Fatalf("rejection = %v, want none", rejection)
	}
	if len(legs) != 2 {
		t.Fatalf("legs = %+v, want 2 legs (FIFO exhausts t1 before touching t2)", legs)
	}
	if legs[0].SourceTokenID != testutil.ID("t1") || legs[0].Amount.GetMinorUnits() != 300 {
		t.Errorf("legs[0] = %+v, want 300 from t1 (fully drained, oldest first)", legs[0])
	}
	if legs[1].SourceTokenID != testutil.ID("t2") || legs[1].Amount.GetMinorUnits() != 100 {
		t.Errorf("legs[1] = %+v, want 100 from t2 (the remainder)", legs[1])
	}
}

func TestSelectSourceTokens_InsufficientBalanceRejects(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(100))
	fundToken(t, lc, testutil.ID("t1"), 100)

	legs, rejection, err := selectSourceTokens(context.Background(), store, lc, testutil.ID("w1"), usd(400), "", nil)
	if err != nil {
		t.Fatalf("selectSourceTokens() error = %v", err)
	}
	if rejection == nil {
		t.Fatalf("rejection = nil, legs = %v, want a rejection", legs)
	}
}

func TestPlanDestinations_AlwaysOneInV1(t *testing.T) {
	t.Parallel()
	specs := planDestinations(testutil.ID("xfer1"), usd(500))
	if len(specs) != 1 {
		t.Fatalf("planDestinations() = %v, want exactly one spec", specs)
	}
	if specs[0].Capacity.GetMinorUnits() != 500 {
		t.Errorf("Capacity = %+v, want 500", specs[0].Capacity)
	}
	if specs[0].TokenID == "" {
		t.Error("TokenID is empty, want an id")
	}
	if again := planDestinations(testutil.ID("xfer1"), usd(500)); again[0].TokenID != specs[0].TokenID {
		t.Errorf("TokenID = %q then %q, want every planning of one Transfer to name the same Token",
			specs[0].TokenID, again[0].TokenID)
	}
	if other := planDestinations(testutil.ID("xfer2"), usd(500)); other[0].TokenID == specs[0].TokenID {
		t.Errorf("two Transfers both planned Token %q, want each its own", other[0].TokenID)
	}
}

func TestReversalManifest_SwapsSourceAndDest(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	transferID := testutil.ID("xfer1")

	if err := store.Append(context.Background(), AggregateType, transferID, 0,
		&pb.TransferRequestAccepted{Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400)},
		&pb.TransferPrepared{Id: transferID, Legs: []*pb.TransferLeg{
			{SourceTokenId: testutil.ID("t1"), DestTokenId: testutil.ID("t-dst"), Amount: usd(300)},
			{SourceTokenId: testutil.ID("t2"), DestTokenId: testutil.ID("t-dst"), Amount: usd(100)},
		}},
		&pb.TransferCommitted{Id: transferID},
	); err != nil {
		t.Fatalf("seed transfer stream: %v", err)
	}

	legs, rejection, err := reversalManifest(context.Background(), store, transferID)
	if err != nil {
		t.Fatalf("reversalManifest() error = %v", err)
	}
	if rejection != nil {
		t.Fatalf("rejection = %v, want none", rejection)
	}
	if len(legs) != 2 {
		t.Fatalf("legs = %+v, want 2", legs)
	}
	if legs[0].SourceTokenID != testutil.ID("t-dst") || legs[0].DestTokenID != testutil.ID("t1") || legs[0].Amount.GetMinorUnits() != 300 {
		t.Errorf("legs[0] = %+v, want source=t-dst dest=t1 amount=300", legs[0])
	}
	if legs[1].SourceTokenID != testutil.ID("t-dst") || legs[1].DestTokenID != testutil.ID("t2") || legs[1].Amount.GetMinorUnits() != 100 {
		t.Errorf("legs[1] = %+v, want source=t-dst dest=t2 amount=100", legs[1])
	}
}

func TestReversalManifest_RejectsUncommittedOriginal(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	transferID := testutil.ID("xfer1")
	if err := store.Append(context.Background(), AggregateType, transferID, 0,
		&pb.TransferRequestAccepted{Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400)},
	); err != nil {
		t.Fatalf("seed transfer stream: %v", err)
	}

	_, rejection, err := reversalManifest(context.Background(), store, transferID)
	if err != nil {
		t.Fatalf("reversalManifest() error = %v", err)
	}
	if rejection == nil {
		t.Fatal("rejection = nil, want a rejection for a non-committed original")
	}
}

func TestReversalManifest_RejectsUnknownTransfer(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()

	_, rejection, err := reversalManifest(context.Background(), store, testutil.ID("never-existed"))
	if err != nil {
		t.Fatalf("reversalManifest() error = %v", err)
	}
	if rejection == nil {
		t.Fatal("rejection = nil, want a rejection for an unknown transfer")
	}
}

// countingLedger counts Balances round trips so a test can prove source
// selection asks TigerBeetle once per Wallet, not once per Token.
type countingLedger struct {
	ledger.Client
	balanceCalls int
}

func (c *countingLedger) Balances(ctx context.Context, ids []string) (map[string]ledger.Balance, error) {
	c.balanceCalls++
	return c.Client.Balances(ctx, ids)
}

func TestSelectSourceTokens_OneLookupForTheWholeWallet(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := &countingLedger{Client: ledger.NewFakeClient()}
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	// Three spent Tokens ahead of the one that can pay: each used to cost
	// its own round trip before selection reached money it could use.
	for _, id := range []string{"t1", "t2", "t3"} {
		mintToken(t, store, lc, testutil.ID("w1"), testutil.ID(id), usd(100))
	}
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t4"), usd(500))
	fundToken(t, lc, testutil.ID("t4"), 500)
	lc.balanceCalls = 0

	legs, rejection, err := selectSourceTokens(context.Background(), store, lc, testutil.ID("w1"), usd(400), "", nil)
	if err != nil || rejection != nil {
		t.Fatalf("selectSourceTokens() = %v, %v; want legs", rejection, err)
	}
	if len(legs) != 1 || legs[0].SourceTokenID != testutil.ID("t4") {
		t.Fatalf("legs = %+v, want one leg from t4", legs)
	}
	if lc.balanceCalls != 1 {
		t.Errorf("Balances called %d times, want 1", lc.balanceCalls)
	}
}

func TestWouldAcceptTransfer_AcceptsWhenFunded(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	rejection, err := WouldAcceptTransfer(context.Background(), store, lc, nil, testutil.ID("w1"), usd(400), "")
	if err != nil {
		t.Fatalf("WouldAcceptTransfer() error = %v", err)
	}
	if rejection != nil {
		t.Fatalf("rejection = %v, want none", rejection)
	}
}

func TestWouldAcceptTransfer_RejectsWhenUnderfunded(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(100))
	fundToken(t, lc, testutil.ID("t1"), 100)

	rejection, err := WouldAcceptTransfer(context.Background(), store, lc, nil, testutil.ID("w1"), usd(400), "")
	if err != nil {
		t.Fatalf("WouldAcceptTransfer() error = %v", err)
	}
	if rejection == nil {
		t.Fatal("rejection = nil, want a rejection for an underfunded wallet")
	}
}

// TestWouldAcceptTransfer_DoesNotMutateAnything is the load-bearing proof
// this check is genuinely side-effect-free: calling it never consumes or
// reserves anything, so a real RequestTransfer against the same wallet
// immediately afterward behaves exactly as if the check had never run.
func TestWouldAcceptTransfer_DoesNotMutateAnything(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	for i := 0; i < 3; i++ {
		if rejection, err := WouldAcceptTransfer(context.Background(), store, lc, nil, testutil.ID("w1"), usd(400), ""); err != nil || rejection != nil {
			t.Fatalf("WouldAcceptTransfer() call %d = rejection=%v err=%v, want accepted every time", i, rejection, err)
		}
	}

	s := NewServer(store, lc, nil, nil)
	resp, err := s.RequestTransfer(context.Background(), &pb.RequestTransferRequest{
		Id: testutil.ID("xfer1"), FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400),
	})
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("result = %v, want TransferRequestAccepted — the repeated dry-run checks above must not have consumed anything", resp.GetResult())
	}
}
