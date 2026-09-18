package ledger

import (
	"context"
	"errors"
	"sync"
)

// ErrBatchEndsInLinkedChain is returned when the last entry of a batch has
// Linked set — a chain has to end on an unlinked entry, so a batch that
// ends mid-chain is a programming error, not a runtime outcome.
var ErrBatchEndsInLinkedChain = errors.New("ledger: batch's last entry has Linked set")

// storedAccount is an Account as FakeClient retains it: economic identity
// (currency, the two mutually-exclusive flags) plus running totals. Linked
// is submission-time metadata, not part of the account's identity, so it is
// deliberately not stored here.
type storedAccount struct {
	currency                   string
	creditsMustNotExceedDebits bool
	debitsMustNotExceedCredits bool

	debitsPosted   uint64
	creditsPosted  uint64
	debitsPending  uint64
	creditsPending uint64
}

func (a storedAccount) matches(other Account) bool {
	return a.currency == other.Currency &&
		a.creditsMustNotExceedDebits == other.Flags.CreditsMustNotExceedDebits &&
		a.debitsMustNotExceedCredits == other.Flags.DebitsMustNotExceedCredits
}

// storedTransfer is a Transfer as FakeClient retains it, for idempotency
// comparison. Linked and Timeout are submission/reservation-lifecycle
// metadata, not part of a transfer's economic identity.
type storedTransfer struct {
	debitAccountID  string
	creditAccountID string
	minorUnits      uint64
	currency        string
	kind            TransferKind
	pendingID       string
}

func newStoredTransfer(t Transfer) storedTransfer {
	return storedTransfer{
		debitAccountID: t.DebitAccountID, creditAccountID: t.CreditAccountID,
		minorUnits: t.MinorUnits, currency: t.Currency, kind: t.Kind, pendingID: t.PendingID,
	}
}

type pendingStatus int

const (
	pendingOpen pendingStatus = iota
	pendingPosted
	pendingVoided
)

type pendingRecord struct {
	debitAccountID  string
	creditAccountID string
	minorUnits      uint64
	status          pendingStatus
}

// FakeClient is an in-memory Client double for tests: full account/transfer
// bookkeeping (including TigerBeetle's two-phase pending transfers and
// flags.Linked batch atomicity), no network, no external process.
type FakeClient struct {
	mu        sync.Mutex
	accounts  map[string]storedAccount
	transfers map[string]storedTransfer
	pending   map[string]*pendingRecord
}

func NewFakeClient() *FakeClient {
	return &FakeClient{
		accounts:  make(map[string]storedAccount),
		transfers: make(map[string]storedTransfer),
		pending:   make(map[string]*pendingRecord),
	}
}

var _ Client = (*FakeClient)(nil)

// linkedRuns groups n items into maximal runs of consecutive indices that
// are chained together: a run continues through every index whose isLinked
// is true and ends at (and includes) the first index whose isLinked is
// false. It is an error for the last item overall to have isLinked true —
// a chain has to end on an unlinked entry.
func linkedRuns(n int, isLinked func(i int) bool) ([][]int, error) {
	if n == 0 {
		return nil, nil
	}
	if isLinked(n - 1) {
		return nil, ErrBatchEndsInLinkedChain
	}
	var runs [][]int
	var current []int
	for i := 0; i < n; i++ {
		current = append(current, i)
		if !isLinked(i) {
			runs = append(runs, current)
			current = nil
		}
	}
	return runs, nil
}

func (c *FakeClient) CreateAccounts(_ context.Context, accounts []Account) ([]AccountResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	runs, err := linkedRuns(len(accounts), func(i int) bool { return accounts[i].Flags.Linked })
	if err != nil {
		return nil, err
	}

	results := make([]AccountResult, len(accounts))
	for _, run := range runs {
		type outcome struct {
			result AccountResultCode
			fresh  bool // true: a brand new account that still needs committing
		}
		outcomes := make([]outcome, len(run))
		failed := false

		for j, i := range run {
			a := accounts[i]
			switch existing, ok := c.accounts[a.ID]; {
			case ok && existing.matches(a):
				outcomes[j] = outcome{result: AccountResultExists}
			case ok:
				outcomes[j] = outcome{result: AccountResultExistsWithDifferentFlags}
				failed = true
			default:
				if _, err := LedgerID(a.Currency); err != nil {
					outcomes[j] = outcome{result: AccountResultInvalid}
					failed = true
					continue
				}
				outcomes[j] = outcome{result: AccountResultOK, fresh: true}
			}
		}

		for j, i := range run {
			switch {
			case failed && outcomes[j].result == AccountResultOK:
				results[i] = AccountResult{Index: i, Result: AccountResultLinkedEventFailed}
			case failed:
				results[i] = AccountResult{Index: i, Result: outcomes[j].result}
			default:
				results[i] = AccountResult{Index: i, Result: outcomes[j].result}
				if outcomes[j].fresh {
					a := accounts[i]
					c.accounts[a.ID] = storedAccount{
						currency:                   a.Currency,
						creditsMustNotExceedDebits: a.Flags.CreditsMustNotExceedDebits,
						debitsMustNotExceedCredits: a.Flags.DebitsMustNotExceedCredits,
					}
				}
			}
		}
	}
	return results, nil
}

func (c *FakeClient) CreateTransfers(_ context.Context, transfers []Transfer) ([]TransferResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	runs, err := linkedRuns(len(transfers), func(i int) bool { return transfers[i].Linked })
	if err != nil {
		return nil, err
	}

	results := make([]TransferResult, len(transfers))
	for _, run := range runs {
		type outcome struct {
			result TransferResultCode
			fresh  bool // true: a brand new, successfully-applied transfer that still needs committing
		}
		outcomes := make([]outcome, len(run))
		failed := false

		// shadow holds tentative account totals for this run only, so a
		// later entry in the same run sees the effect of an earlier one,
		// without mutating real state until the whole run is known to
		// succeed.
		shadow := make(map[string]storedAccount)
		shadowPending := make(map[string]pendingStatus)
		getAccount := func(id string) (storedAccount, bool) {
			if a, ok := shadow[id]; ok {
				return a, true
			}
			a, ok := c.accounts[id]
			if ok {
				shadow[id] = a
			}
			return a, ok
		}
		pendingStatusFor := func(id string) (pendingStatus, bool) {
			if s, ok := shadowPending[id]; ok {
				return s, true
			}
			rec, ok := c.pending[id]
			if !ok {
				return pendingOpen, false
			}
			return rec.status, true
		}

		for j, i := range run {
			t := transfers[i]

			if existing, ok := c.transfers[t.ID]; ok {
				if existing == newStoredTransfer(t) {
					outcomes[j] = outcome{result: TransferResultExists}
				} else {
					outcomes[j] = outcome{result: TransferResultExistsWithDifferentFields}
					failed = true
				}
				continue
			}

			debit, debitOK := getAccount(t.DebitAccountID)
			credit, creditOK := getAccount(t.CreditAccountID)
			if !debitOK || !creditOK {
				outcomes[j] = outcome{result: TransferResultAccountNotFound}
				failed = true
				continue
			}

			switch t.Kind {
			case TransferKindRegular:
				debit.debitsPosted += t.MinorUnits
				credit.creditsPosted += t.MinorUnits
			case TransferKindPending:
				debit.debitsPending += t.MinorUnits
				credit.creditsPending += t.MinorUnits
			case TransferKindPostPending, TransferKindVoidPending:
				status, ok := pendingStatusFor(t.PendingID)
				rec := c.pending[t.PendingID]
				matches := ok && status == pendingOpen && rec != nil &&
					rec.debitAccountID == t.DebitAccountID && rec.creditAccountID == t.CreditAccountID &&
					rec.minorUnits == t.MinorUnits
				if !matches {
					outcomes[j] = outcome{result: TransferResultPendingTransferNotFound}
					failed = true
					continue
				}
				debit.debitsPending -= t.MinorUnits
				credit.creditsPending -= t.MinorUnits
				if t.Kind == TransferKindPostPending {
					debit.debitsPosted += t.MinorUnits
					credit.creditsPosted += t.MinorUnits
					shadowPending[t.PendingID] = pendingPosted
				} else {
					shadowPending[t.PendingID] = pendingVoided
				}
			}

			if debit.debitsMustNotExceedCredits && debit.debitsPosted+debit.debitsPending > debit.creditsPosted+debit.creditsPending {
				outcomes[j] = outcome{result: TransferResultExceedsCredits}
				failed = true
				continue
			}
			if credit.creditsMustNotExceedDebits && credit.creditsPosted+credit.creditsPending > credit.debitsPosted+credit.debitsPending {
				outcomes[j] = outcome{result: TransferResultExceedsDebits}
				failed = true
				continue
			}

			shadow[t.DebitAccountID] = debit
			shadow[t.CreditAccountID] = credit
			outcomes[j] = outcome{result: TransferResultOK, fresh: true}
		}

		for j, i := range run {
			switch {
			case failed && outcomes[j].result == TransferResultOK:
				results[i] = TransferResult{Index: i, Result: TransferResultLinkedEventFailed}
			case failed:
				results[i] = TransferResult{Index: i, Result: outcomes[j].result}
			default:
				results[i] = TransferResult{Index: i, Result: outcomes[j].result}
				if outcomes[j].fresh {
					t := transfers[i]
					c.transfers[t.ID] = newStoredTransfer(t)
					c.accounts[t.DebitAccountID] = shadow[t.DebitAccountID]
					c.accounts[t.CreditAccountID] = shadow[t.CreditAccountID]
					switch t.Kind {
					case TransferKindPending:
						c.pending[t.ID] = &pendingRecord{
							debitAccountID: t.DebitAccountID, creditAccountID: t.CreditAccountID,
							minorUnits: t.MinorUnits, status: pendingOpen,
						}
					case TransferKindPostPending:
						c.pending[t.PendingID].status = pendingPosted
					case TransferKindVoidPending:
						c.pending[t.PendingID].status = pendingVoided
					}
				}
			}
		}
	}
	return results, nil
}

func (c *FakeClient) Balances(_ context.Context, accountIDs []string) (map[string]Balance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	balances := make(map[string]Balance, len(accountIDs))
	for _, id := range accountIDs {
		a, ok := c.accounts[id]
		if !ok {
			continue
		}
		balances[id] = Balance{
			Currency:     a.currency,
			DebitsPosted: a.debitsPosted, CreditsPosted: a.creditsPosted,
			DebitsPending: a.debitsPending, CreditsPending: a.creditsPending,
		}
	}
	return balances, nil
}
