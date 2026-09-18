package ledger_test

import (
	"context"
	"testing"
	"uuid"

	"github.com/namelessnotion/money_flow/go/internal/ledger"
)

func newID(t *testing.T) string {
	t.Helper()
	return uuid.NewV7().String()
}

func newAccount(t *testing.T, flags ledger.AccountFlags) ledger.Account {
	t.Helper()
	return ledger.Account{ID: newID(t), Currency: "USD", Flags: flags}
}

func mustCreateAccounts(t *testing.T, c ledger.Client, accounts ...ledger.Account) {
	t.Helper()
	results, err := c.CreateAccounts(context.Background(), accounts)
	if err != nil {
		t.Fatalf("CreateAccounts: %v", err)
	}
	for i, r := range results {
		if r.Result != ledger.AccountResultOK {
			t.Fatalf("CreateAccounts[%d]: want OK, got %v", i, r.Result)
		}
	}
}

func TestFakeClient_CreateAccounts_Succeeds(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	a := newAccount(t, ledger.AccountFlags{})

	results, err := c.CreateAccounts(context.Background(), []ledger.Account{a})
	if err != nil {
		t.Fatalf("CreateAccounts: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.AccountResultOK {
		t.Fatalf("got %+v, want single OK", results)
	}
}

func TestFakeClient_CreateAccounts_IdempotentRetry(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	a := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, a)

	results, err := c.CreateAccounts(context.Background(), []ledger.Account{a})
	if err != nil {
		t.Fatalf("CreateAccounts: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.AccountResultExists {
		t.Fatalf("got %+v, want single Exists", results)
	}
}

func TestFakeClient_CreateAccounts_ConflictingRetryDiffersFlags(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	a := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, a)

	conflicting := a
	conflicting.Flags = ledger.AccountFlags{CreditsMustNotExceedDebits: true}
	results, err := c.CreateAccounts(context.Background(), []ledger.Account{conflicting})
	if err != nil {
		t.Fatalf("CreateAccounts: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.AccountResultExistsWithDifferentFlags {
		t.Fatalf("got %+v, want single ExistsWithDifferentFlags", results)
	}
}

func TestFakeClient_CreateAccounts_LinkedBatchAllSucceed(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	a1 := newAccount(t, ledger.AccountFlags{Linked: true})
	a2 := newAccount(t, ledger.AccountFlags{})

	results, err := c.CreateAccounts(context.Background(), []ledger.Account{a1, a2})
	if err != nil {
		t.Fatalf("CreateAccounts: %v", err)
	}
	for i, r := range results {
		if r.Result != ledger.AccountResultOK {
			t.Fatalf("results[%d] = %v, want OK", i, r.Result)
		}
	}
}

func TestFakeClient_CreateAccounts_LinkedBatchOneFailsFailsAll(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	good := newAccount(t, ledger.AccountFlags{Linked: true})
	bad := ledger.Account{ID: newID(t), Currency: "NOT_A_CURRENCY", Flags: ledger.AccountFlags{}}

	results, err := c.CreateAccounts(context.Background(), []ledger.Account{good, bad})
	if err != nil {
		t.Fatalf("CreateAccounts: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Result != ledger.AccountResultLinkedEventFailed {
		t.Errorf("results[0] = %v, want LinkedEventFailed", results[0].Result)
	}
	if results[1].Result == ledger.AccountResultOK {
		t.Errorf("results[1] = OK, want a failure code")
	}

	// Neither account should have been created.
	if _, found, _ := ledger.AccountBalance(context.Background(), c, good.ID); found {
		t.Errorf("good account %q was created despite linked failure", good.ID)
	}
}

func TestFakeClient_CreateAccounts_LinkedOnLastEntryIsRejected(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	a := newAccount(t, ledger.AccountFlags{Linked: true})

	if _, err := c.CreateAccounts(context.Background(), []ledger.Account{a}); err == nil {
		t.Fatalf("want an error for a batch whose last entry has Linked set")
	}
}

func TestFakeClient_CreateTransfers_RegularPostsImmediately(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit, credit)

	xfer := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindRegular,
	}
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{xfer})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultOK {
		t.Fatalf("got %+v, want single OK", results)
	}

	balance, found, err := ledger.AccountBalance(context.Background(), c, credit.ID)
	if err != nil || !found {
		t.Fatalf("AccountBalance(credit): found=%v err=%v", found, err)
	}
	if balance != 500 {
		t.Errorf("credit balance = %d, want 500", balance)
	}

	balance, found, err = ledger.AccountBalance(context.Background(), c, debit.ID)
	if err != nil || !found {
		t.Fatalf("AccountBalance(debit): found=%v err=%v", found, err)
	}
	// no flags set, so the debit account is free to go negative
	if balance != negate(500) {
		t.Errorf("debit balance = %d, want -500", balance)
	}
}

func negate(u uint64) int64 { return -int64(u) }

func TestFakeClient_CreateTransfers_ExceedsDebitsRejectsCredit(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	// A wallet that can only onramp (e.g. Debit Card): credits_must_not_exceed_debits.
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{CreditsMustNotExceedDebits: true})
	mustCreateAccounts(t, c, debit, credit)

	xfer := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindRegular,
	}
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{xfer})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultExceedsDebits {
		t.Fatalf("got %+v, want single ExceedsDebits", results)
	}

	if balance, found, _ := ledger.AccountBalance(context.Background(), c, credit.ID); found && balance != 0 {
		t.Errorf("credit balance = %d, want 0 (transfer should not have applied)", balance)
	}
}

func TestFakeClient_CreateTransfers_UnknownAccountRejected(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit)

	xfer := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: newID(t),
		MinorUnits: 100, Currency: "USD", Kind: ledger.TransferKindRegular,
	}
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{xfer})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultAccountNotFound {
		t.Fatalf("got %+v, want single AccountNotFound", results)
	}
}

func TestFakeClient_CreateTransfers_PendingReservesWithoutPosting(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit, credit)

	xfer := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPending, Timeout: 3600,
	}
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{xfer})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultOK {
		t.Fatalf("got %+v, want single OK", results)
	}

	balance, found, err := ledger.AccountBalance(context.Background(), c, credit.ID)
	if err != nil || !found {
		t.Fatalf("AccountBalance(credit): found=%v err=%v", found, err)
	}
	if balance != 0 {
		t.Errorf("credit posted balance = %d, want 0 (still pending)", balance)
	}
}

func TestFakeClient_CreateTransfers_PendingEnforcesFlagsAgainstReservation(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{CreditsMustNotExceedDebits: true})
	mustCreateAccounts(t, c, debit, credit)

	xfer := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPending,
	}
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{xfer})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultExceedsDebits {
		t.Fatalf("got %+v, want single ExceedsDebits (reservation counts toward the invariant)", results)
	}
}

func TestFakeClient_CreateTransfers_PostPendingFinalizesIntoPosted(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit, credit)

	pendingID := newID(t)
	pending := ledger.Transfer{
		ID: pendingID, DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPending,
	}
	if _, err := c.CreateTransfers(context.Background(), []ledger.Transfer{pending}); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	post := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPostPending, PendingID: pendingID,
	}
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{post})
	if err != nil {
		t.Fatalf("CreateTransfers(post): %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultOK {
		t.Fatalf("got %+v, want single OK", results)
	}

	balance, found, err := ledger.AccountBalance(context.Background(), c, credit.ID)
	if err != nil || !found {
		t.Fatalf("AccountBalance(credit): found=%v err=%v", found, err)
	}
	if balance != 500 {
		t.Errorf("credit posted balance = %d, want 500", balance)
	}

	// Idempotent retry of the same post converges rather than double-posting.
	results, err = c.CreateTransfers(context.Background(), []ledger.Transfer{post})
	if err != nil {
		t.Fatalf("CreateTransfers(post retry): %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultExists {
		t.Fatalf("got %+v, want single Exists", results)
	}
	if balance, _, _ := ledger.AccountBalance(context.Background(), c, credit.ID); balance != 500 {
		t.Errorf("credit posted balance after retry = %d, want 500 (no double-post)", balance)
	}
}

func TestFakeClient_CreateTransfers_PostPendingUnknownPendingIDRejected(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit, credit)

	post := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPostPending, PendingID: newID(t),
	}
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{post})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultPendingTransferNotFound {
		t.Fatalf("got %+v, want single PendingTransferNotFound", results)
	}
}

func TestFakeClient_CreateTransfers_VoidPendingReleasesReservation(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit, credit)

	pendingID := newID(t)
	pending := ledger.Transfer{
		ID: pendingID, DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPending,
	}
	if _, err := c.CreateTransfers(context.Background(), []ledger.Transfer{pending}); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	void := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindVoidPending, PendingID: pendingID,
	}
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{void})
	if err != nil {
		t.Fatalf("CreateTransfers(void): %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultOK {
		t.Fatalf("got %+v, want single OK", results)
	}

	if balance, _, _ := ledger.AccountBalance(context.Background(), c, credit.ID); balance != 0 {
		t.Errorf("credit posted balance = %d, want 0 (voided, never posted)", balance)
	}

	// Idempotent retry converges.
	results, err = c.CreateTransfers(context.Background(), []ledger.Transfer{void})
	if err != nil {
		t.Fatalf("CreateTransfers(void retry): %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultExists {
		t.Fatalf("got %+v, want single Exists", results)
	}

	// Once voided, a matching post should no longer find a live pending transfer.
	post := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPostPending, PendingID: pendingID,
	}
	results, err = c.CreateTransfers(context.Background(), []ledger.Transfer{post})
	if err != nil {
		t.Fatalf("CreateTransfers(post after void): %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultPendingTransferNotFound {
		t.Fatalf("got %+v, want single PendingTransferNotFound", results)
	}
}

func TestFakeClient_CreateTransfers_LinkedBatchOneFailsAppliesNone(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit, credit)

	good := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindRegular, Linked: true,
	}
	bad := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: newID(t), // unknown credit account
		MinorUnits: 100, Currency: "USD", Kind: ledger.TransferKindRegular,
	}

	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{good, bad})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Result != ledger.TransferResultLinkedEventFailed {
		t.Errorf("results[0] = %v, want LinkedEventFailed", results[0].Result)
	}
	if results[1].Result != ledger.TransferResultAccountNotFound {
		t.Errorf("results[1] = %v, want AccountNotFound", results[1].Result)
	}

	if balance, _, _ := ledger.AccountBalance(context.Background(), c, credit.ID); balance != 0 {
		t.Errorf("credit balance = %d, want 0 (linked batch should not have applied)", balance)
	}
}

func TestFakeClient_CreateTransfers_DuplicateWithDifferentFieldsRejected(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit, credit)

	id := newID(t)
	first := ledger.Transfer{
		ID: id, DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindRegular,
	}
	if _, err := c.CreateTransfers(context.Background(), []ledger.Transfer{first}); err != nil {
		t.Fatalf("create first: %v", err)
	}

	conflicting := first
	conflicting.MinorUnits = 999
	results, err := c.CreateTransfers(context.Background(), []ledger.Transfer{conflicting})
	if err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultExistsWithDifferentFields {
		t.Fatalf("got %+v, want single ExistsWithDifferentFields", results)
	}
}

func TestFakeClient_AccountBalance_NotFound(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	_, found, err := ledger.AccountBalance(context.Background(), c, newID(t))
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if found {
		t.Errorf("found = true for an account that was never created")
	}
}

func TestFakeClient_Balances_ReportsPostedAndPendingPerAccount(t *testing.T) {
	t.Parallel()
	c := ledger.NewFakeClient()
	debit := newAccount(t, ledger.AccountFlags{})
	credit := newAccount(t, ledger.AccountFlags{})
	mustCreateAccounts(t, c, debit, credit)

	pending := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 500, Currency: "USD", Kind: ledger.TransferKindPending, Timeout: 3600,
	}
	posted := ledger.Transfer{
		ID: newID(t), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
		MinorUnits: 200, Currency: "USD", Kind: ledger.TransferKindRegular,
	}
	if _, err := c.CreateTransfers(context.Background(), []ledger.Transfer{pending, posted}); err != nil {
		t.Fatalf("CreateTransfers: %v", err)
	}

	unknown := newID(t)
	balances, err := c.Balances(context.Background(), []string{debit.ID, credit.ID, unknown})
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	if got, want := balances[debit.ID], (ledger.Balance{Currency: "USD", DebitsPosted: 200, DebitsPending: 500}); got != want {
		t.Errorf("debit = %+v, want %+v", got, want)
	}
	if got, want := balances[credit.ID], (ledger.Balance{Currency: "USD", CreditsPosted: 200, CreditsPending: 500}); got != want {
		t.Errorf("credit = %+v, want %+v", got, want)
	}
	if _, ok := balances[unknown]; ok {
		t.Errorf("an account that was never created must be absent from the result")
	}
}

func TestBalance_PostedNet(t *testing.T) {
	t.Parallel()
	if net, err := (ledger.Balance{CreditsPosted: 200, DebitsPosted: 500, CreditsPending: 900}).PostedNet(); err != nil || net != -300 {
		t.Errorf("PostedNet = %d, %v; want -300, nil (pending excluded, negative allowed)", net, err)
	}
	if _, err := (ledger.Balance{CreditsPosted: 1 << 63}).PostedNet(); err == nil {
		t.Errorf("PostedNet: want an overflow error for credits beyond int64")
	}
}
