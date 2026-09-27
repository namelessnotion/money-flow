package ledger

import "fmt"

// String names the outcome in the snake_case an operator sees in logs and
// metrics, matching the TigerBeetle result it was mapped from where there is one.
func (c AccountResultCode) String() string {
	switch c {
	case AccountResultUnspecified:
		return "unspecified"
	case AccountResultOK:
		return "ok"
	case AccountResultExists:
		return "exists"
	case AccountResultExistsWithDifferentFlags:
		return "exists_with_different_flags"
	case AccountResultLinkedEventFailed:
		return "linked_event_failed"
	case AccountResultInvalid:
		return "invalid"
	default:
		return fmt.Sprintf("AccountResultCode(%d)", int(c))
	}
}

// String names the outcome in the snake_case an operator sees in logs and
// metrics, matching the TigerBeetle result it was mapped from where there is one.
func (c TransferResultCode) String() string {
	switch c {
	case TransferResultUnspecified:
		return "unspecified"
	case TransferResultOK:
		return "ok"
	case TransferResultExists:
		return "exists"
	case TransferResultExistsWithDifferentFields:
		return "exists_with_different_fields"
	case TransferResultExceedsCredits:
		return "exceeds_credits"
	case TransferResultExceedsDebits:
		return "exceeds_debits"
	case TransferResultLinkedEventFailed:
		return "linked_event_failed"
	case TransferResultPendingTransferNotFound:
		return "pending_transfer_not_found"
	case TransferResultAccountNotFound:
		return "account_not_found"
	case TransferResultInvalid:
		return "invalid"
	default:
		return fmt.Sprintf("TransferResultCode(%d)", int(c))
	}
}
