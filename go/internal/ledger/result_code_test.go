package ledger_test

import (
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/ledger"
)

// Result codes are how an operator reads a ledger rejection in a log line or a
// metric, so each one names itself rather than printing as a bare integer.
func TestTransferResultCode_NamesEveryOutcome(t *testing.T) {
	t.Parallel()
	for code, want := range map[ledger.TransferResultCode]string{
		ledger.TransferResultUnspecified:               "unspecified",
		ledger.TransferResultOK:                        "ok",
		ledger.TransferResultExists:                    "exists",
		ledger.TransferResultExistsWithDifferentFields: "exists_with_different_fields",
		ledger.TransferResultExceedsCredits:            "exceeds_credits",
		ledger.TransferResultExceedsDebits:             "exceeds_debits",
		ledger.TransferResultLinkedEventFailed:         "linked_event_failed",
		ledger.TransferResultPendingTransferNotFound:   "pending_transfer_not_found",
		ledger.TransferResultAccountNotFound:           "account_not_found",
		ledger.TransferResultInvalid:                   "invalid",
		ledger.TransferResultCode(99):                  "TransferResultCode(99)",
	} {
		if got := code.String(); got != want {
			t.Errorf("TransferResultCode(%d).String() = %q, want %q", int(code), got, want)
		}
	}
}

func TestAccountResultCode_NamesEveryOutcome(t *testing.T) {
	t.Parallel()
	for code, want := range map[ledger.AccountResultCode]string{
		ledger.AccountResultUnspecified:              "unspecified",
		ledger.AccountResultOK:                       "ok",
		ledger.AccountResultExists:                   "exists",
		ledger.AccountResultExistsWithDifferentFlags: "exists_with_different_flags",
		ledger.AccountResultLinkedEventFailed:        "linked_event_failed",
		ledger.AccountResultInvalid:                  "invalid",
		ledger.AccountResultCode(99):                 "AccountResultCode(99)",
	} {
		if got := code.String(); got != want {
			t.Errorf("AccountResultCode(%d).String() = %q, want %q", int(code), got, want)
		}
	}
}
