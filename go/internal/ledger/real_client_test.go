package ledger_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
	"uuid"

	"github.com/namelessnotion/money_flow/go/internal/ledger"
)

const defaultTestTigerBeetleAddress = "127.0.0.1:3000"

// testRealClient connects to the dockerized TigerBeetle, skipping the test
// (or benchmark — testing.TB covers both) when it isn't reachable so
// `go test ./...` still works without Docker running, mirroring eventstore's
// testPool.
func testRealClient(t testing.TB) *ledger.RealClient {
	t.Helper()

	addr := os.Getenv("TIGERBEETLE_ADDRESS")
	if addr == "" {
		addr = defaultTestTigerBeetleAddress
	}
	clusterID := uint64(0)
	if v := os.Getenv("TIGERBEETLE_CLUSTER_ID"); v != "" {
		parsed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("TIGERBEETLE_CLUSTER_ID=%q: %v", v, err)
		}
		clusterID = parsed
	}

	type dialed struct {
		client *ledger.RealClient
		err    error
	}
	ch := make(chan dialed, 1)
	go func() {
		c, err := ledger.NewRealClient(clusterID, []string{addr})
		if err != nil {
			ch <- dialed{err: err}
			return
		}
		if _, err := c.CreateAccounts(context.Background(), nil); err != nil {
			c.Close()
			ch <- dialed{err: err}
			return
		}
		ch <- dialed{client: c}
	}()

	select {
	case d := <-ch:
		if d.err != nil {
			t.Skipf("skipping: no TigerBeetle at %s (run `docker compose up -d tigerbeetle`): %v", addr, d.err)
		}
		t.Cleanup(d.client.Close)
		return d.client
	case <-time.After(2 * time.Second):
		t.Skipf("skipping: no TigerBeetle at %s within 2s (run `docker compose up -d tigerbeetle`)", addr)
	}
	return nil
}

func TestRealClient_CreateAccountsAndTransfer(t *testing.T) {
	c := testRealClient(t)
	ctx := context.Background()

	debit := ledger.Account{ID: uuid.NewV7().String(), Currency: "USD", Flags: ledger.AccountFlags{}}
	credit := ledger.Account{ID: uuid.NewV7().String(), Currency: "USD", Flags: ledger.AccountFlags{}}

	accountResults, err := c.CreateAccounts(ctx, []ledger.Account{debit, credit})
	if err != nil {
		t.Fatalf("CreateAccounts: %v", err)
	}
	for i, r := range accountResults {
		if r.Result != ledger.AccountResultOK {
			t.Fatalf("account %d: got %v, want OK", i, r.Result)
		}
	}

	xfer := ledger.Transfer{
		ID: uuid.NewV7().String(), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 1234, Currency: "USD", Kind: ledger.TransferKindRegular,
	}
	transferResults, err := c.CreateTransfers(ctx, []ledger.Transfer{xfer})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(transferResults) != 1 || transferResults[0].Result != ledger.TransferResultOK {
		t.Fatalf("got %+v, want single OK", transferResults)
	}

	balance, found, err := ledger.AccountBalance(ctx, c, credit.ID)
	if err != nil || !found {
		t.Fatalf("AccountBalance(credit): found=%v err=%v", found, err)
	}
	if balance != 1234 {
		t.Errorf("credit balance = %d, want 1234", balance)
	}
}

func TestRealClient_PendingTransferLifecycle(t *testing.T) {
	c := testRealClient(t)
	ctx := context.Background()

	debit := ledger.Account{ID: uuid.NewV7().String(), Currency: "USD", Flags: ledger.AccountFlags{}}
	credit := ledger.Account{ID: uuid.NewV7().String(), Currency: "USD", Flags: ledger.AccountFlags{}}
	if _, err := c.CreateAccounts(ctx, []ledger.Account{debit, credit}); err != nil {
		t.Fatalf("CreateAccounts: %v", err)
	}

	pendingID := uuid.NewV7().String()
	pending := ledger.Transfer{
		ID: pendingID, DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPending, Timeout: 3600,
	}
	if results, err := c.CreateTransfers(ctx, []ledger.Transfer{pending}); err != nil {
		t.Fatalf("CreateTransfers(pending): %v", err)
	} else if results[0].Result != ledger.TransferResultOK {
		t.Fatalf("pending: got %v, want OK", results[0].Result)
	}

	if balance, found, err := ledger.AccountBalance(ctx, c, credit.ID); err != nil || !found || balance != 0 {
		t.Fatalf("AccountBalance(credit) after pending: balance=%d found=%v err=%v, want 0/true/nil", balance, found, err)
	}
	balances, err := c.Balances(ctx, []string{debit.ID, credit.ID})
	if err != nil {
		t.Fatalf("Balances after pending: %v", err)
	}
	if got, want := balances[debit.ID], (ledger.Balance{Currency: "USD", DebitsPending: 500}); got != want {
		t.Errorf("debit after pending = %+v, want %+v", got, want)
	}
	if got, want := balances[credit.ID], (ledger.Balance{Currency: "USD", CreditsPending: 500}); got != want {
		t.Errorf("credit after pending = %+v, want %+v", got, want)
	}

	post := ledger.Transfer{
		ID: uuid.NewV7().String(), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPostPending, PendingID: pendingID,
	}
	if results, err := c.CreateTransfers(ctx, []ledger.Transfer{post}); err != nil {
		t.Fatalf("CreateTransfers(post): %v", err)
	} else if results[0].Result != ledger.TransferResultOK {
		t.Fatalf("post: got %v, want OK", results[0].Result)
	}

	if balance, found, err := ledger.AccountBalance(ctx, c, credit.ID); err != nil || !found || balance != 500 {
		t.Fatalf("AccountBalance(credit) after post: balance=%d found=%v err=%v, want 500/true/nil", balance, found, err)
	}
}
