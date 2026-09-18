package token

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	walletpb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

func openWallet(t *testing.T, store eventstore.Store, walletID string, allows sharedpb.Allows) {
	t.Helper()
	_, err := wallet.NewServer(store).Open(context.Background(), &walletpb.OpenRequest{
		Id: walletID, HolderId: testutil.ID("h1"), Name: "test wallet", Allows: allows,
	})
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}
}

func mintRequest(id, walletID string) *pb.MintRequest {
	return &pb.MintRequest{
		Id: id, WalletId: walletID,
		Capacity: &sharedpb.Money{MinorUnits: 1000, Currency: "USD"},
	}
}

func TestMint_Succeeds(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)

	server := NewServer(store, lc)
	resp, err := server.Mint(context.Background(), mintRequest(testutil.ID("t1"), testutil.ID("w1")))
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	minted := resp.GetTokenMinted()
	if minted == nil {
		t.Fatalf("result = %v, want TokenMinted", resp.GetResult())
	}
	if minted.GetId() != testutil.ID("t1") || minted.GetWalletId() != testutil.ID("w1") {
		t.Errorf("TokenMinted = (%q, %q), want (%s, %s)", minted.GetId(), minted.GetWalletId(), testutil.ID("t1"), testutil.ID("w1"))
	}

	// TigerBeetle account actually exists.
	if _, found, err := ledger.AccountBalance(context.Background(), lc, testutil.ID("t1")); err != nil || !found {
		t.Errorf("AccountBalance: found=%v err=%v, want a TigerBeetle account to exist", found, err)
	}

	// The Wallet's own stream records TokenMintedForWallet.
	tokens, err := wallet.TokensOf(context.Background(), store, testutil.ID("w1"))
	if err != nil {
		t.Fatalf("TokensOf() error = %v", err)
	}
	if len(tokens) != 1 || tokens[0] != testutil.ID("t1") {
		t.Errorf("TokensOf() = %v, want [%s]", tokens, testutil.ID("t1"))
	}
}

func TestMint_IsIdempotent(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	server := NewServer(store, lc)
	ctx := context.Background()

	first, err := server.Mint(ctx, mintRequest(testutil.ID("t1"), testutil.ID("w1")))
	if err != nil {
		t.Fatalf("first Mint() error = %v", err)
	}
	second, err := server.Mint(ctx, mintRequest(testutil.ID("t1"), testutil.ID("w1")))
	if err != nil {
		t.Fatalf("second Mint() error = %v", err)
	}
	if first.GetTokenMinted().GetId() != second.GetTokenMinted().GetId() {
		t.Errorf("replay returned a different token: %v vs %v", first, second)
	}

	tokens, err := wallet.TokensOf(ctx, store, testutil.ID("w1"))
	if err != nil {
		t.Fatalf("TokensOf() error = %v", err)
	}
	if len(tokens) != 1 {
		t.Errorf("TokensOf() = %v, want exactly one token (no duplicate mint)", tokens)
	}
}

func TestMint_RejectsUnknownWallet(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	server := NewServer(store, lc)

	resp, err := server.Mint(context.Background(), mintRequest(testutil.ID("t1"), testutil.ID("w-missing")))
	if err != nil {
		t.Fatalf("Mint() error = %v, want a domain rejection instead", err)
	}
	rejected := resp.GetTokenMintRejected()
	if rejected == nil {
		t.Fatalf("result = %v, want TokenMintRejected", resp.GetResult())
	}
	if rejected.GetId() != testutil.ID("t1") {
		t.Errorf("TokenMintRejected.Id = %q, want %q", rejected.GetId(), testutil.ID("t1"))
	}
}

func TestMint_ValidatesRequest(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	server := NewServer(store, lc)
	ctx := context.Background()

	tests := []struct {
		name string
		req  *pb.MintRequest
	}{
		{"missing id", &pb.MintRequest{WalletId: testutil.ID("w1"), Capacity: &sharedpb.Money{MinorUnits: 1, Currency: "USD"}}},
		{"non-uuid id", &pb.MintRequest{Id: "not-a-uuid", WalletId: testutil.ID("w1"), Capacity: &sharedpb.Money{MinorUnits: 1, Currency: "USD"}}},
		{"missing wallet_id", &pb.MintRequest{Id: testutil.ID("t1"), Capacity: &sharedpb.Money{MinorUnits: 1, Currency: "USD"}}},
		{"missing capacity", &pb.MintRequest{Id: testutil.ID("t1"), WalletId: testutil.ID("w1")}},
		{"missing currency", &pb.MintRequest{Id: testutil.ID("t1"), WalletId: testutil.ID("w1"), Capacity: &sharedpb.Money{MinorUnits: 1}}},
		{"zero minor_units", &pb.MintRequest{Id: testutil.ID("t1"), WalletId: testutil.ID("w1"), Capacity: &sharedpb.Money{Currency: "USD"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := server.Mint(ctx, tt.req); err == nil {
				t.Errorf("Mint(%+v) error = nil, want a validation error", tt.req)
			}
		})
	}
}

func TestMint_RetriesOnWalletStreamRace(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)

	// Simulate a concurrent Mint of a *different* Token into the same
	// Wallet winning the race for the Wallet stream's next sequence, on the
	// first attempt only.
	racer := NewServer(store, lc)
	wrapped := &conflictOnceStore{Store: store, onConflict: func() {
		if _, err := racer.Mint(context.Background(), mintRequest(testutil.ID("t-other"), testutil.ID("w1"))); err != nil {
			t.Errorf("seeding racing mint: %v", err)
		}
	}}

	server := NewServer(wrapped, lc)
	resp, err := server.Mint(context.Background(), mintRequest(testutil.ID("t1"), testutil.ID("w1")))
	if err != nil {
		t.Fatalf("Mint() error = %v, want it to retry past the wallet-stream race", err)
	}
	if resp.GetTokenMinted().GetId() != testutil.ID("t1") {
		t.Fatalf("result = %v, want TokenMinted for %s", resp.GetResult(), testutil.ID("t1"))
	}

	tokens, err := wallet.TokensOf(context.Background(), store, testutil.ID("w1"))
	if err != nil {
		t.Fatalf("TokensOf() error = %v", err)
	}
	want := []string{testutil.ID("t-other"), testutil.ID("t1")}
	if len(tokens) != len(want) || tokens[0] != want[0] || tokens[1] != want[1] {
		t.Errorf("TokensOf() = %v, want %v (racer's token first, then this one)", tokens, want)
	}
}

// conflictOnceStore forces the first append to lose an optimistic-
// concurrency race, running onConflict first so the winning event is
// already in the log — mirrors holder's test double of the same name. Both
// append methods are overridden: Mint goes through AppendAtomic and
// RecordBalances through Append.
type conflictOnceStore struct {
	eventstore.Store
	onConflict func()
	conflicted bool
}

func (s *conflictOnceStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	return s.AppendAtomic(ctx, eventstore.StreamWrite{
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		ExpectedSeq:   expectedSeq,
		Events:        events,
	})
}

func (s *conflictOnceStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	if !s.conflicted {
		s.conflicted = true
		s.onConflict()
		return eventstore.ErrConcurrencyConflict
	}
	return s.Store.AppendAtomic(ctx, writes...)
}
