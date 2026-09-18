package token

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// mintedToken opens a Wallet and mints one Token into it, returning the
// Token's id.
func mintedToken(t *testing.T, store eventstore.Store, lc ledger.Client, name string, allows sharedpb.Allows) string {
	t.Helper()
	walletID, tokenID := testutil.ID("w-"+name), testutil.ID(name)
	openWallet(t, store, walletID, allows)
	resp, err := NewServer(store, lc).Mint(context.Background(), mintRequest(tokenID, walletID))
	if err != nil || resp.GetTokenMinted() == nil {
		t.Fatalf("mint %s: %v %v", name, resp, err)
	}
	return tokenID
}

// move submits one TigerBeetle transfer between two Tokens.
func move(t *testing.T, lc ledger.Client, id, from, to string, minorUnits uint64, kind ledger.TransferKind) {
	t.Helper()
	results, err := lc.CreateTransfers(context.Background(), []ledger.Transfer{{
		ID: testutil.ID(id), DebitAccountID: from, CreditAccountID: to,
		MinorUnits: minorUnits, Currency: "USD", Kind: kind, Timeout: 3600,
	}})
	if err != nil || results[0].Result != ledger.TransferResultOK {
		t.Fatalf("transfer %s: %v %v", id, results, err)
	}
}

// recorded returns every TokenBalanceRecorded on tokenID's stream, oldest
// first.
func recorded(t *testing.T, store eventstore.Store, tokenID string) []*pb.TokenBalanceRecorded {
	t.Helper()
	events, err := store.Load(context.Background(), AggregateType, tokenID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var out []*pb.TokenBalanceRecorded
	for _, e := range events {
		msg, err := e.Decode()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r, ok := msg.(*pb.TokenBalanceRecorded); ok {
			out = append(out, r)
		}
	}
	return out
}

func TestRecordBalances_PublishesPostedAndPendingForEachToken(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bank := mintedToken(t, store, lc, "bank", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	cash := mintedToken(t, store, lc, "cash", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	move(t, lc, "posted", bank, cash, 300, ledger.TransferKindRegular)
	move(t, lc, "staged", bank, cash, 50, ledger.TransferKindPending)

	if err := RecordBalances(context.Background(), store, lc, []string{bank, cash}); err != nil {
		t.Fatalf("RecordBalances: %v", err)
	}

	wantBank := &pb.TokenBalanceRecorded{
		Id: bank, WalletId: testutil.ID("w-bank"), Currency: "USD",
		PostedMinorUnits: -300, PendingOutgoingMinorUnits: 50,
	}
	if got := recorded(t, store, bank); len(got) != 1 || !proto.Equal(got[0], wantBank) {
		t.Errorf("bank recorded %v, want [%v] (a bank Token may go negative)", got, wantBank)
	}
	wantCash := &pb.TokenBalanceRecorded{
		Id: cash, WalletId: testutil.ID("w-cash"), Currency: "USD",
		PostedMinorUnits: 300, PendingIncomingMinorUnits: 50,
	}
	if got := recorded(t, store, cash); len(got) != 1 || !proto.Equal(got[0], wantCash) {
		t.Errorf("cash recorded %v, want [%v]", got, wantCash)
	}
}

func TestRecordBalances_UnchangedBalanceIsNotRecordedAgain(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bank := mintedToken(t, store, lc, "bank", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	cash := mintedToken(t, store, lc, "cash", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	move(t, lc, "posted", bank, cash, 300, ledger.TransferKindRegular)

	// A retried saga step records again after TigerBeetle reports Exists.
	for range 2 {
		if err := RecordBalances(context.Background(), store, lc, []string{cash, cash}); err != nil {
			t.Fatalf("RecordBalances: %v", err)
		}
	}

	if got := recorded(t, store, cash); len(got) != 1 {
		t.Errorf("recorded %d TokenBalanceRecorded, want 1", len(got))
	}
}

func TestRecordBalances_LosingARaceReReadsTheLedger(t *testing.T) {
	t.Parallel()
	inner := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bank := mintedToken(t, inner, lc, "bank", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	cash := mintedToken(t, inner, lc, "cash", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	move(t, lc, "first", bank, cash, 300, ledger.TransferKindRegular)

	// Between this caller's ledger read and its append, a second Transfer
	// lands and its own publisher records the balance first.
	store := &conflictOnceStore{Store: inner}
	store.onConflict = func() {
		move(t, lc, "second", bank, cash, 200, ledger.TransferKindRegular)
		if err := RecordBalances(context.Background(), inner, lc, []string{cash}); err != nil {
			t.Fatalf("racing RecordBalances: %v", err)
		}
	}

	if err := RecordBalances(context.Background(), store, lc, []string{cash}); err != nil {
		t.Fatalf("RecordBalances: %v", err)
	}

	got := recorded(t, inner, cash)
	if len(got) == 0 || got[len(got)-1].GetPostedMinorUnits() != 500 {
		t.Errorf("last recorded = %v, want posted 500: a stale 300 must never land last", got)
	}
}

func TestRecordBalances_UnmintedTokenIsAnError(t *testing.T) {
	t.Parallel()
	err := RecordBalances(context.Background(), eventstore.NewMemoryStore(), ledger.NewFakeClient(), []string{testutil.ID("ghost")})
	if err == nil {
		t.Fatal("RecordBalances: want an error for a Token that was never minted")
	}
}
