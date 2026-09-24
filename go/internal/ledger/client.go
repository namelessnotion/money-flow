// Package ledger wraps TigerBeetle: the source of truth for account
// balances, as opposed to eventstore which is the source of truth for
// intent. Token and Operation ids are used directly as TigerBeetle's
// 128-bit account/transfer IDs, so callers into this package must pass
// valid UUIDs.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// BatchMax is the most accounts or transfers one CreateAccounts or
// CreateTransfers call may carry. It is TigerBeetle's per-request ceiling
// under the smallest request size any replica we run uses: `--development`
// (docker/tigerbeetle/entrypoint.sh) shrinks a request to 32 KiB, which after
// the message header and the multi-batch trailer holds 253 128-byte events —
// measured against 0.17.9, and pinned by
// TestRealClient_BatchMaxLinkedTransfersFitOneRequest. A production replica's
// 1 MiB request holds 8189, so this is conservative there, deliberately:
// every replica in a cluster must share one batch size, and the code must run
// against both.
//
// A linked chain cannot span requests, so BatchMax is also the longest chain
// any caller can submit. A caller with more to apply atomically has to build
// that atomicity itself (go/docs/adr/0008).
const BatchMax = 253

// ErrInvalidRequest marks a CreateAccounts or CreateTransfers call refused as
// a whole, before anything was applied, for a reason no retry can change: a
// batch over BatchMax, a batch ending mid-chain, a malformed id, an unknown
// currency, or TigerBeetle itself reporting the request too large. Contrast a
// per-entry rejection, which comes back as a result code, and a transport
// error, which is worth retrying.
var ErrInvalidRequest = errors.New("ledger: invalid request")

// ErrBatchTooLarge is ErrInvalidRequest for a batch over BatchMax.
var ErrBatchTooLarge = fmt.Errorf("%w: batch exceeds BatchMax (%d)", ErrInvalidRequest, BatchMax)

// checkBatchSize refuses a batch over BatchMax.
func checkBatchSize(n int) error {
	if n > BatchMax {
		return fmt.Errorf("%w: got %d", ErrBatchTooLarge, n)
	}
	return nil
}

// AccountFlags mirrors the subset of TigerBeetle account flags this domain
// needs. Linked chains this account's creation to the next one in the same
// CreateAccounts call so they succeed or fail together; it must be false on
// the last account of any batch. CreditsMustNotExceedDebits and
// DebitsMustNotExceedCredits are mutually exclusive and are set at Token
// mint time from the owning Wallet's Allows policy.
type AccountFlags struct {
	Linked                     bool
	CreditsMustNotExceedDebits bool
	DebitsMustNotExceedCredits bool
}

// Account is a TigerBeetle account to create, backing one Token.
type Account struct {
	ID       string // valid UUID; used as the TigerBeetle account id
	Currency string // resolved to TigerBeetle's numeric ledger via currency.go
	Flags    AccountFlags
}

// AccountResultCode is the outcome of creating one Account.
type AccountResultCode int

const (
	AccountResultUnspecified AccountResultCode = iota
	// AccountResultOK means the account was created.
	AccountResultOK
	// AccountResultExists means an account with this id already exists with
	// identical fields — treated as success by callers (idempotent retry).
	AccountResultExists
	// AccountResultExistsWithDifferentFlags means an account with this id
	// already exists but with different fields — a real conflict, not a
	// safe retry.
	AccountResultExistsWithDifferentFlags
	// AccountResultLinkedEventFailed means this account was part of a
	// flags.Linked chain and a different entry in the chain failed, so this
	// one was not created either.
	AccountResultLinkedEventFailed
	// AccountResultInvalid means the account's own fields are malformed
	// (e.g. an unrecognized currency) — it was rejected on its own merits,
	// not because of a sibling in a linked chain.
	AccountResultInvalid
)

// AccountResult reports the outcome of creating one Account, at the same
// index it was submitted at.
type AccountResult struct {
	Index  int
	Result AccountResultCode
}

// TransferKind selects which of TigerBeetle's transfer semantics this
// Transfer uses. Regular posts immediately. Pending reserves capacity
// (debits_pending/credits_pending) without posting it — used for a staged
// Transfer's initial DEBIT batch. PostPending finalizes a prior Pending
// transfer into posted balance. VoidPending releases a prior Pending
// transfer's reservation without posting anything.
type TransferKind int

const (
	TransferKindUnspecified TransferKind = iota
	TransferKindRegular
	TransferKindPending
	TransferKindPostPending
	TransferKindVoidPending
)

// Transfer is a TigerBeetle transfer to submit, one leg of an Operation.
type Transfer struct {
	ID              string // valid UUID; used as the TigerBeetle transfer id
	DebitAccountID  string
	CreditAccountID string
	MinorUnits      uint64
	Currency        string
	Linked          bool
	Kind            TransferKind

	// PendingID is required for TransferKindPostPending and
	// TransferKindVoidPending: the id of the TransferKindPending transfer
	// being finalized or released. Empty for other kinds.
	PendingID string

	// Timeout is how long a TransferKindPending reservation is held before
	// TigerBeetle auto-voids it, if it is never posted or voided first.
	// Zero means no timeout. Ignored for other kinds.
	Timeout int64 // seconds
}

// TransferResultCode is the outcome of submitting one Transfer.
type TransferResultCode int

const (
	TransferResultUnspecified TransferResultCode = iota
	// TransferResultOK means the transfer was applied (posted, reserved,
	// finalized, or voided, depending on Kind).
	TransferResultOK
	// TransferResultExists means a transfer with this id already exists
	// with identical fields — treated as success by callers.
	TransferResultExists
	// TransferResultExistsWithDifferentFields means a transfer with this id
	// already exists but with different fields — a real conflict.
	TransferResultExistsWithDifferentFields
	// TransferResultExceedsCredits means the debit account's
	// debits_must_not_exceed_credits flag would be violated.
	TransferResultExceedsCredits
	// TransferResultExceedsDebits means the credit account's
	// credits_must_not_exceed_debits flag would be violated.
	TransferResultExceedsDebits
	// TransferResultLinkedEventFailed means this transfer was part of a
	// flags.Linked chain and a different entry in the chain failed.
	TransferResultLinkedEventFailed
	// TransferResultPendingTransferNotFound means Kind was PostPending or
	// VoidPending but PendingID does not name a live pending transfer.
	TransferResultPendingTransferNotFound
	// TransferResultAccountNotFound means DebitAccountID or CreditAccountID
	// does not name an existing account.
	TransferResultAccountNotFound
	// TransferResultInvalid means the transfer's own fields are malformed
	// (e.g. a zero amount, mismatched ledgers, a missing required field) —
	// rejected on its own merits, not because of a sibling in a linked
	// chain and not one of the specific outcomes above.
	TransferResultInvalid
)

// TransferResult reports the outcome of submitting one Transfer, at the
// same index it was submitted at.
type TransferResult struct {
	Index  int
	Result TransferResultCode
}

// Client is TigerBeetle's boundary: the two batch-submission calls every
// mint and every Transfer saga step goes through, plus a batched balance read.
// Implementations must be safe for concurrent use.
type Client interface {
	// CreateAccounts submits a batch of account-creation requests. Entries
	// with Flags.Linked set are chained to the following entry so the whole
	// run of linked entries succeeds or fails together; the last entry in
	// any linked chain must not set Flags.Linked. At most BatchMax entries;
	// an error wrapping ErrInvalidRequest means nothing was applied and
	// resubmitting the same batch cannot succeed.
	CreateAccounts(ctx context.Context, accounts []Account) ([]AccountResult, error)

	// CreateTransfers submits a batch of transfers. Entries with Linked set
	// are chained the same way as CreateAccounts, under the same BatchMax
	// and ErrInvalidRequest contract.
	CreateTransfers(ctx context.Context, transfers []Transfer) ([]TransferResult, error)

	// Balances looks up every given account in one round trip (chunked
	// internally at TigerBeetle's per-request limit) and returns each one's
	// posted and pending totals, keyed by account id. An account that
	// doesn't exist is simply absent from the map.
	Balances(ctx context.Context, accountIDs []string) (map[string]Balance, error)
}

// Balance is one account's four running totals as TigerBeetle keeps them.
// Pending amounts are reservations made by TransferKindPending that a later
// PostPending moves into the posted totals or a VoidPending releases.
type Balance struct {
	Currency       string
	DebitsPosted   uint64
	CreditsPosted  uint64
	DebitsPending  uint64
	CreditsPending uint64
}

// PostedNet is posted credits minus posted debits, excluding any pending
// reservation. Signed because an account with neither flag set (free to
// move in either direction) can go negative.
func (b Balance) PostedNet() (int64, error) {
	if b.CreditsPosted > math.MaxInt64 || b.DebitsPosted > math.MaxInt64 {
		return 0, fmt.Errorf("ledger: posted totals %d/%d overflow int64", b.CreditsPosted, b.DebitsPosted)
	}
	return int64(b.CreditsPosted) - int64(b.DebitsPosted), nil
}

// AccountBalance returns one account's PostedNet, or found=false if no such
// account exists.
func AccountBalance(ctx context.Context, c Client, accountID string) (minorUnits int64, found bool, err error) {
	balances, err := c.Balances(ctx, []string{accountID})
	if err != nil {
		return 0, false, err
	}
	b, ok := balances[accountID]
	if !ok {
		return 0, false, nil
	}
	net, err := b.PostedNet()
	if err != nil {
		return 0, false, fmt.Errorf("ledger: account %q: %w", accountID, err)
	}
	return net, true, nil
}
