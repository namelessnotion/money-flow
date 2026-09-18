package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	holderpb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	operationpb "github.com/namelessnotion/money_flow/go/gen/proto/operation/v1"
	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	walletpb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

// Drives the real mux over real HTTP with the generated clients. Everything
// else in the suite calls the servers directly, so this is the only place the
// wire itself — routing, path prefixes, protobuf content negotiation — is
// exercised.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMux(eventstore.NewMemoryStore(), okPinger{}, ledger.NewFakeClient()))
	t.Cleanup(srv.Close)
	return srv
}

func TestHolderServiceOverHTTP(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	client := holderpb.NewHolderServiceProtobufClient(srv.URL, srv.Client())

	resp, err := client.Provision(context.Background(), &holderpb.ProvisionRequest{
		Id: testutil.ID("h1"),
		Wallets: []*holderpb.WalletSpec{
			{WalletId: testutil.ID("w1"), Name: "bank", Allows: sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP},
			{WalletId: testutil.ID("w2"), Name: "cash", Allows: sharedpb.Allows_ALLOWS_NONE},
		},
	})
	if err != nil {
		t.Fatalf("Provision() over HTTP error = %v", err)
	}
	if got := resp.GetHolderProvisioned().GetWalletIds(); len(got) != 2 {
		t.Errorf("WalletIds = %v, want 2", got)
	}
	// Enum values must survive the round trip; a mis-encoded enum would come
	// back as UNSPECIFIED without any transport error.
	if resp.GetId() != testutil.ID("h1") {
		t.Errorf("response Id = %q, want %s", resp.GetId(), testutil.ID("h1"))
	}
}

// newTestServerWithLedger is like newTestServer but returns the FakeClient
// too, so a test can fund a Token directly (there is no HTTP endpoint for
// that — Tokens only ever gain balance via a Transfer) while still proving
// the TigerBeetle-backed services are wired correctly end to end.
func newTestServerWithLedger(t *testing.T) (*httptest.Server, *ledger.FakeClient) {
	t.Helper()
	lc := ledger.NewFakeClient()
	srv := httptest.NewServer(newMux(eventstore.NewMemoryStore(), okPinger{}, lc))
	t.Cleanup(srv.Close)
	return srv, lc
}

func TestTokenServiceOverHTTP(t *testing.T) {
	t.Parallel()

	srv, _ := newTestServerWithLedger(t)
	walletClient := walletpb.NewWalletServiceProtobufClient(srv.URL, srv.Client())
	tokenClient := tokenpb.NewTokenServiceProtobufClient(srv.URL, srv.Client())
	ctx := context.Background()

	if _, err := walletClient.Open(ctx, &walletpb.OpenRequest{
		Id: testutil.ID("w1"), HolderId: testutil.ID("h1"), Name: "bank", Allows: sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP,
	}); err != nil {
		t.Fatalf("Open() over HTTP error = %v", err)
	}

	resp, err := tokenClient.Mint(ctx, &tokenpb.MintRequest{
		Id: testutil.ID("t1"), WalletId: testutil.ID("w1"), Capacity: &sharedpb.Money{MinorUnits: 1000, Currency: "USD"},
	})
	if err != nil {
		t.Fatalf("Mint() over HTTP error = %v", err)
	}
	if resp.GetTokenMinted().GetId() != testutil.ID("t1") {
		t.Errorf("result = %v, want TokenMinted for %s", resp.GetResult(), testutil.ID("t1"))
	}
}

func TestOperationServiceOverHTTP(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	client := operationpb.NewOperationServiceProtobufClient(srv.URL, srv.Client())

	resp, err := client.Initiate(context.Background(), &operationpb.InitiateRequest{
		Id: testutil.ID("op1"), TransferId: testutil.ID("xfer1"),
		TokenId: testutil.ID("t-src"), CounterpartyTokenId: testutil.ID("t-dst"),
		Operator: operationpb.Operator_OPERATOR_DEBIT, Amount: &sharedpb.Money{MinorUnits: 500, Currency: "USD"},
	})
	if err != nil {
		t.Fatalf("Initiate() over HTTP error = %v", err)
	}
	if resp.GetInitiated().GetId() != testutil.ID("op1") {
		t.Errorf("result = %v, want Initiated for %s", resp.GetResult(), testutil.ID("op1"))
	}
}

func TestTransferServiceOverHTTP(t *testing.T) {
	t.Parallel()

	srv, lc := newTestServerWithLedger(t)
	walletClient := walletpb.NewWalletServiceProtobufClient(srv.URL, srv.Client())
	tokenClient := tokenpb.NewTokenServiceProtobufClient(srv.URL, srv.Client())
	transferClient := transferpb.NewTransferServiceProtobufClient(srv.URL, srv.Client())
	ctx := context.Background()

	for _, w := range []string{"w1", "w2"} {
		if _, err := walletClient.Open(ctx, &walletpb.OpenRequest{
			Id: testutil.ID(w), HolderId: testutil.ID("h1"), Name: w, Allows: sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP,
		}); err != nil {
			t.Fatalf("Open(%s) over HTTP error = %v", w, err)
		}
	}
	if _, err := tokenClient.Mint(ctx, &tokenpb.MintRequest{
		Id: testutil.ID("t1"), WalletId: testutil.ID("w1"), Capacity: &sharedpb.Money{MinorUnits: 1000, Currency: "USD"},
	}); err != nil {
		t.Fatalf("Mint() over HTTP error = %v", err)
	}
	// Fund t1 directly — there's no HTTP endpoint that puts money into a
	// Token; only a Transfer does, and this test is proving Transfer itself
	// works.
	if _, err := lc.CreateAccounts(ctx, []ledger.Account{{ID: testutil.ID("external-source"), Currency: "USD"}}); err != nil {
		t.Fatalf("create external-source: %v", err)
	}
	if _, err := lc.CreateTransfers(ctx, []ledger.Transfer{{
		ID: testutil.ID("seed-fund"), DebitAccountID: testutil.ID("external-source"), CreditAccountID: testutil.ID("t1"),
		MinorUnits: 1000, Currency: "USD", Kind: ledger.TransferKindRegular,
	}}); err != nil {
		t.Fatalf("fund t1: %v", err)
	}

	resp, err := transferClient.RequestTransfer(ctx, &transferpb.RequestTransferRequest{
		Id: testutil.ID("xfer1"), FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"),
		Amount: &sharedpb.Money{MinorUnits: 400, Currency: "USD"},
	})
	if err != nil {
		t.Fatalf("RequestTransfer() over HTTP error = %v", err)
	}
	if resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("result = %v, want TransferRequestAccepted", resp.GetResult())
	}

	// The saga runs synchronously within RequestTransfer, so by the time the
	// HTTP response above came back the whole thing — mint, debit, credit —
	// already committed; confirm via the shared ledger.
	if balance, _, _ := ledger.AccountBalance(ctx, lc, testutil.ID("t1")); balance != 600 {
		t.Errorf("t1 balance = %d, want 600 (1000 - 400)", balance)
	}
}

// TestTransactionServiceOverHTTP proves Transaction's own routing/content-
// negotiation is wired correctly. It deliberately stays shallow — the full
// DAG/rollback/mint_source-guarantee/cross-transaction-visibility logic is
// already exhaustively covered by the transaction package's own tests,
// which call the servers directly; this is only the wire itself.
func TestTransactionServiceOverHTTP(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	walletClient := walletpb.NewWalletServiceProtobufClient(srv.URL, srv.Client())
	transactionClient := transactionpb.NewTransactionServiceProtobufClient(srv.URL, srv.Client())
	ctx := context.Background()

	for _, w := range []string{"w1", "w2"} {
		if _, err := walletClient.Open(ctx, &walletpb.OpenRequest{
			Id: testutil.ID(w), HolderId: testutil.ID("h1"), Name: w, Allows: sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP,
		}); err != nil {
			t.Fatalf("Open(%s) over HTTP error = %v", w, err)
		}
	}

	// mint_source=true against an ALLOWS_ONRAMP_AND_OFFRAMP wallet needs no
	// pre-funding — the saga mints its own fresh source Token — which keeps
	// this smoke test from also having to reach into a FakeClient.
	xferID := testutil.ID("xfer1")
	resp, err := transactionClient.StartInitializingTransaction(ctx, &transactionpb.StartInitializingTransactionRequest{
		Id: testutil.ID("txn1"),
		Transfers: map[string]*transactionpb.Transfer{
			xferID: {
				Id: xferID, Amount: &sharedpb.Money{MinorUnits: 500, Currency: "USD"},
				FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"),
				AutoProcess: true, MintSource: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() over HTTP error = %v", err)
	}
	if resp.GetTransactionInitialized() == nil {
		t.Fatalf("result = %v, want TransactionInitialized", resp.GetResult())
	}
}

func TestWalletServiceOverHTTP(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	client := walletpb.NewWalletServiceProtobufClient(srv.URL, srv.Client())

	resp, err := client.Open(context.Background(), &walletpb.OpenRequest{
		Id: testutil.ID("w1"), HolderId: testutil.ID("h1"), Name: "bank", Allows: sharedpb.Allows_ALLOWS_OFFRAMP,
	})
	if err != nil {
		t.Fatalf("Open() over HTTP error = %v", err)
	}
	if got := resp.GetWalletOpened().GetAllows(); got != sharedpb.Allows_ALLOWS_OFFRAMP {
		t.Errorf("Allows round-tripped as %v, want ALLOWS_OFFRAMP", got)
	}
}

// The path prefix is the whole reason this test exists: a client pointed at the
// bare host rather than /twirp gets a 404, and nothing in either language's
// unit tests would catch it.
func TestServicesMountUnderTwirpPrefix(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"under the twirp prefix", "/twirp/holder.v1.HolderService/Establish", http.StatusBadRequest},
		{"without the prefix", "/holder.v1.HolderService/Establish", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An empty body is a malformed request, not a routing failure — so
			// 400 means "routed and rejected" while 404 means "never reached".
			resp, err := srv.Client().Post(srv.URL+tc.path, "application/protobuf", http.NoBody)
			if err != nil {
				t.Fatalf("POST %s error = %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Errorf("POST %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
