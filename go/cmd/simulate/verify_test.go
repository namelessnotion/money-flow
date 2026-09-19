package main

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	walletpb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/token"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

func TestLastRecordedBalanceReturnsTheMostRecentOne(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := eventstore.NewMemoryStore()
	tokenID := testutil.ID("t1")

	if _, found, err := lastRecordedBalance(mustLoad(t, store, token.AggregateType, tokenID)); err != nil || found {
		t.Fatalf("lastRecordedBalance(no events) = (_, %v, %v), want found=false, err=nil", found, err)
	}

	if err := store.Append(ctx, token.AggregateType, tokenID, 0,
		&tokenpb.TokenMinted{Id: tokenID, WalletId: testutil.ID("w1")},
		&tokenpb.TokenBalanceRecorded{Id: tokenID, PostedMinorUnits: 100},
		&tokenpb.TokenBalanceRecorded{Id: tokenID, PostedMinorUnits: 250},
	); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, found, err := lastRecordedBalance(mustLoad(t, store, token.AggregateType, tokenID))
	if err != nil {
		t.Fatalf("lastRecordedBalance: %v", err)
	}
	if !found {
		t.Fatal("lastRecordedBalance: found = false, want true")
	}
	if got != 250 {
		t.Errorf("lastRecordedBalance = %d, want 250 (the last one recorded)", got)
	}
}

// mintedToken opens a Wallet (if not already open) and mints one Token into
// it, mirroring token/balance_test.go's own helper of the same shape: Mint
// only creates a zero-balance TigerBeetle account, so a test after real
// ledger activity still has to move money into it separately (see move).
func mintedToken(t *testing.T, store eventstore.Store, lc ledger.Client, walletID, tokenSeed string) string {
	t.Helper()
	ctx := context.Background()
	tokenID := testutil.ID(tokenSeed)
	resp, err := token.NewServer(store, lc).Mint(ctx, &tokenpb.MintRequest{
		Id: tokenID, WalletId: walletID, Capacity: &sharedpb.Money{MinorUnits: 1, Currency: "USD"},
	})
	if err != nil || resp.GetTokenMinted() == nil {
		t.Fatalf("mint %s: %v %v", tokenSeed, resp, err)
	}
	return tokenID
}

// move submits one TigerBeetle transfer between two Tokens, crediting to
// with amount debited from from.
func move(t *testing.T, lc ledger.Client, seed, from, to string, minorUnits uint64) {
	t.Helper()
	results, err := lc.CreateTransfers(context.Background(), []ledger.Transfer{{
		ID: testutil.ID(seed), DebitAccountID: from, CreditAccountID: to,
		MinorUnits: minorUnits, Currency: "USD", Kind: ledger.TransferKindRegular,
	}})
	if err != nil || results[0].Result != ledger.TransferResultOK {
		t.Fatalf("transfer %s: %v %v", seed, results, err)
	}
}

func TestWalletBalanceSumsAcrossEveryToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	walletID := testutil.ID("w1")
	source := testutil.ID("external-source") // an account outside any Wallet, standing in for a mint source

	if _, err := wallet.NewServer(store).Open(ctx, &walletpb.OpenRequest{
		Id: walletID, HolderId: testutil.ID("h1"), Name: "main", Allows: sharedpb.Allows_ALLOWS_NONE,
	}); err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	if _, err := lc.CreateAccounts(ctx, []ledger.Account{{ID: source, Currency: "USD"}}); err != nil {
		t.Fatalf("create source account: %v", err)
	}

	t1 := mintedToken(t, store, lc, walletID, "t1")
	t2 := mintedToken(t, store, lc, walletID, "t2")
	move(t, lc, "credit-t1", source, t1, 100)
	move(t, lc, "credit-t2", source, t2, 200)
	if err := token.RecordBalances(ctx, store, lc, []string{t1, t2}); err != nil {
		t.Fatalf("RecordBalances: %v", err)
	}

	got, err := walletBalance(ctx, store, walletID)
	if err != nil {
		t.Fatalf("walletBalance: %v", err)
	}
	if got != 300 {
		t.Errorf("walletBalance = %d, want 300 (100 + 200)", got)
	}
}

func TestReconcileFlagsAMismatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	source := testutil.ID("external-source")

	walletID := testutil.ID("w1")
	if _, err := wallet.NewServer(store).Open(ctx, &walletpb.OpenRequest{
		Id: walletID, HolderId: testutil.ID("h1"), Name: "main", Allows: sharedpb.Allows_ALLOWS_NONE,
	}); err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	if _, err := lc.CreateAccounts(ctx, []ledger.Account{{ID: source, Currency: "USD"}}); err != nil {
		t.Fatalf("create source account: %v", err)
	}
	tok := mintedToken(t, store, lc, walletID, "t1")
	move(t, lc, "credit", source, tok, 500)
	if err := token.RecordBalances(ctx, store, lc, []string{tok}); err != nil {
		t.Fatalf("RecordBalances: %v", err)
	}

	entities := []entity{{name: "entity-0", walletID: walletID}}
	results, err := reconcile(ctx, store, entities, map[string]int64{walletID: 999})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("reconcile returned %d results, want 1", len(results))
	}
	if results[0].ok() {
		t.Error("reconcile: ok() = true, want false (expected 999, actual 500)")
	}

	matched, err := reconcile(ctx, store, entities, map[string]int64{walletID: 500})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !matched[0].ok() {
		t.Errorf("reconcile: ok() = false, want true (expected and actual both 500)")
	}
}

func mustLoad(t *testing.T, store eventstore.Store, aggregateType, id string) []eventstore.Event {
	t.Helper()
	events, err := store.Load(context.Background(), aggregateType, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return events
}
