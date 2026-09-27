package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/telemetry"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// A transfer TigerBeetle refuses on its merits is the ledger doing its job.
// The call succeeded; the refusal is counted by its code, so an operator can
// see a rise in exceeds_credits without it reading as a ledger outage.
func TestLedger_ARejectedTransferIsCountedByItsCodeNotTreatedAsAFault(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	lc := telemetry.NewLedger(ledger.NewFakeClient(), rec.Providers)
	ctx := context.Background()
	payer, payee := testutil.ID("payer"), testutil.ID("payee")

	if _, err := lc.CreateAccounts(ctx, []ledger.Account{
		{ID: payer, Currency: "USD", Flags: ledger.AccountFlags{DebitsMustNotExceedCredits: true}},
		{ID: payee, Currency: "USD"},
	}); err != nil {
		t.Fatalf("CreateAccounts() error = %v", err)
	}
	results, err := lc.CreateTransfers(ctx, []ledger.Transfer{{
		ID: testutil.ID("overdraw"), DebitAccountID: payer, CreditAccountID: payee,
		MinorUnits: 100, Currency: "USD", Kind: ledger.TransferKindRegular,
	}})
	if err != nil {
		t.Fatalf("CreateTransfers() error = %v", err)
	}
	if len(results) != 1 || results[0].Result != ledger.TransferResultExceedsCredits {
		t.Fatalf("results = %+v, want one exceeds_credits passed through", results)
	}

	span := rec.span(t, "ledger.create_transfers")
	wantAttr(t, span, "money_flow.ledger.batch_size", "1")
	wantAttr(t, span, "money_flow.ledger.rejected", "1")
	if span.Status().Code != codes.Unset {
		t.Errorf("status = %v, want Unset", span.Status().Code)
	}
	if n := rec.sum(t, "money_flow.ledger.rejections",
		attribute.String("money_flow.operation", "create_transfers"),
		attribute.String("money_flow.ledger.result", "exceeds_credits")); n != 1 {
		t.Errorf("exceeds_credits rejections = %d, want 1", n)
	}
	if n := rec.sum(t, "money_flow.ledger.rejections",
		attribute.String("money_flow.operation", "create_accounts")); n != 0 {
		t.Errorf("create_accounts rejections = %d, want 0: both accounts were created", n)
	}
	if n := rec.histogramCount(t, "money_flow.ledger.operation.duration",
		attribute.String("money_flow.operation", "create_transfers"),
		attribute.String("money_flow.outcome", "ok")); n != 1 {
		t.Errorf("create_transfers duration observations = %d, want 1", n)
	}
}

// A batch the ledger refused as a whole is a caller bug that no retry can fix,
// so it is recorded as a failure with its own outcome.
func TestLedger_AnInvalidRequestIsAnError(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	lc := telemetry.NewLedger(ledger.NewFakeClient(), rec.Providers)

	_, err := lc.CreateTransfers(context.Background(), make([]ledger.Transfer, ledger.BatchMax+1))
	if !errors.Is(err, ledger.ErrInvalidRequest) {
		t.Fatalf("CreateTransfers() error = %v, want ErrInvalidRequest passed through", err)
	}

	span := rec.span(t, "ledger.create_transfers")
	wantAttr(t, span, "money_flow.outcome", "invalid_request")
	if span.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", span.Status().Code)
	}
}

func TestLedger_BalancesAreTraced(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	lc := telemetry.NewLedger(ledger.NewFakeClient(), rec.Providers)

	if _, err := lc.Balances(context.Background(), []string{testutil.ID("a"), testutil.ID("b")}); err != nil {
		t.Fatalf("Balances() error = %v", err)
	}

	wantAttr(t, rec.span(t, "ledger.balances"), "money_flow.ledger.batch_size", "2")
}
