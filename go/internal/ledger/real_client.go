package ledger

import (
	"context"
	"fmt"
	"net"
	"uuid"

	tb "github.com/tigerbeetle/tigerbeetle-go"
)

// netLookupIP is net.LookupIP behind a package variable so tests can reason
// about what the resolver actually offered.
var netLookupIP = net.LookupIP

// resolveClusterAddress converts a host:port address into ip:port.
// tigerbeetle-go parses cluster addresses itself and accepts only numeric
// IPs — handing it a DNS name (a Docker Compose service name such as
// "tigerbeetle:3000", say) fails with "invalid client cluster address" rather
// than resolving it — so resolve here, at the one place that dials. An
// already-numeric address is passed through untouched. IPv4 is preferred when
// a host offers both, matching how TigerBeetle is routinely deployed.
func resolveClusterAddress(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("ledger: tigerbeetle address %q must be host:port: %w", addr, err)
	}
	if net.ParseIP(host) != nil {
		return addr, nil
	}
	ips, err := netLookupIP(host)
	if err != nil {
		return "", fmt.Errorf("ledger: resolve tigerbeetle host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("ledger: tigerbeetle host %q resolved to no addresses", host)
	}
	chosen := ips[0]
	for _, ip := range ips {
		if ip.To4() != nil {
			chosen = ip
			break
		}
	}
	return net.JoinHostPort(chosen.String(), port), nil
}

// RealClient wraps the tigerbeetle-go client, translating between this
// package's domain types and TigerBeetle's wire types.
type RealClient struct {
	tb tb.Client
}

var _ Client = (*RealClient)(nil)

// NewRealClient dials TigerBeetle at the given addresses under the given
// cluster id.
func NewRealClient(clusterID uint64, addresses []string) (*RealClient, error) {
	resolved := make([]string, len(addresses))
	for i, addr := range addresses {
		r, err := resolveClusterAddress(addr)
		if err != nil {
			return nil, err
		}
		resolved[i] = r
	}
	c, err := tb.NewClient(tb.ToUint128(clusterID), resolved)
	if err != nil {
		return nil, fmt.Errorf("ledger: connect to tigerbeetle: %w", err)
	}
	return &RealClient{tb: c}, nil
}

// Close releases the underlying TigerBeetle client's resources.
func (c *RealClient) Close() {
	c.tb.Close()
}

func idToUint128(id string) (tb.Uint128, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return tb.Uint128{}, fmt.Errorf("ledger: %q is not a valid UUID: %w", id, err)
	}
	return tb.BytesToUint128(u), nil
}

func (c *RealClient) CreateAccounts(_ context.Context, accounts []Account) ([]AccountResult, error) {
	tbAccounts := make([]tb.Account, len(accounts))
	for i, a := range accounts {
		id, err := idToUint128(a.ID)
		if err != nil {
			return nil, err
		}
		ledgerID, err := LedgerID(a.Currency)
		if err != nil {
			return nil, fmt.Errorf("ledger: account %q: %w", a.ID, err)
		}
		flags := tb.AccountFlags{
			Linked:                     a.Flags.Linked,
			CreditsMustNotExceedDebits: a.Flags.CreditsMustNotExceedDebits,
			DebitsMustNotExceedCredits: a.Flags.DebitsMustNotExceedCredits,
		}
		tbAccounts[i] = tb.Account{
			ID:     id,
			Ledger: ledgerID,
			Code:   code,
			Flags:  flags.ToUint16(),
		}
	}

	results, err := c.tb.CreateAccounts(tbAccounts)
	if err != nil {
		return nil, fmt.Errorf("ledger: CreateAccounts: %w", err)
	}
	out := make([]AccountResult, len(results))
	for i, r := range results {
		out[i] = AccountResult{Index: i, Result: accountResultCode(r.Status)}
	}
	return out, nil
}

func accountResultCode(status tb.CreateAccountStatus) AccountResultCode {
	switch status {
	case tb.AccountCreated:
		return AccountResultOK
	case tb.AccountExists:
		return AccountResultExists
	case tb.AccountExistsWithDifferentFlags,
		tb.AccountExistsWithDifferentUserData128,
		tb.AccountExistsWithDifferentUserData64,
		tb.AccountExistsWithDifferentUserData32,
		tb.AccountExistsWithDifferentLedger,
		tb.AccountExistsWithDifferentCode:
		return AccountResultExistsWithDifferentFlags
	case tb.AccountLinkedEventFailed:
		return AccountResultLinkedEventFailed
	default:
		return AccountResultInvalid
	}
}

func (c *RealClient) CreateTransfers(_ context.Context, transfers []Transfer) ([]TransferResult, error) {
	tbTransfers := make([]tb.Transfer, len(transfers))
	for i, t := range transfers {
		id, err := idToUint128(t.ID)
		if err != nil {
			return nil, err
		}
		debitID, err := idToUint128(t.DebitAccountID)
		if err != nil {
			return nil, err
		}
		creditID, err := idToUint128(t.CreditAccountID)
		if err != nil {
			return nil, err
		}
		ledgerID, err := LedgerID(t.Currency)
		if err != nil {
			return nil, fmt.Errorf("ledger: transfer %q: %w", t.ID, err)
		}

		var pendingID tb.Uint128
		if t.PendingID != "" {
			pendingID, err = idToUint128(t.PendingID)
			if err != nil {
				return nil, err
			}
		}

		flags := tb.TransferFlags{
			Linked:              t.Linked,
			Pending:             t.Kind == TransferKindPending,
			PostPendingTransfer: t.Kind == TransferKindPostPending,
			VoidPendingTransfer: t.Kind == TransferKindVoidPending,
		}

		tbTransfers[i] = tb.Transfer{
			ID:              id,
			DebitAccountID:  debitID,
			CreditAccountID: creditID,
			Amount:          tb.ToUint128(t.MinorUnits),
			PendingID:       pendingID,
			Timeout:         uint32(t.Timeout),
			Ledger:          ledgerID,
			Code:            code,
			Flags:           flags.ToUint16(),
		}
	}

	results, err := c.tb.CreateTransfers(tbTransfers)
	if err != nil {
		return nil, fmt.Errorf("ledger: CreateTransfers: %w", err)
	}
	out := make([]TransferResult, len(results))
	for i, r := range results {
		out[i] = TransferResult{Index: i, Result: transferResultCode(r.Status)}
	}
	return out, nil
}

func transferResultCode(status tb.CreateTransferStatus) TransferResultCode {
	switch status {
	case tb.TransferCreated:
		return TransferResultOK
	case tb.TransferExists:
		return TransferResultExists
	case tb.TransferExistsWithDifferentFlags,
		tb.TransferExistsWithDifferentPendingID,
		tb.TransferExistsWithDifferentTimeout,
		tb.TransferExistsWithDifferentDebitAccountID,
		tb.TransferExistsWithDifferentCreditAccountID,
		tb.TransferExistsWithDifferentAmount,
		tb.TransferExistsWithDifferentUserData128,
		tb.TransferExistsWithDifferentUserData64,
		tb.TransferExistsWithDifferentUserData32,
		tb.TransferExistsWithDifferentLedger,
		tb.TransferExistsWithDifferentCode:
		return TransferResultExistsWithDifferentFields
	case tb.TransferLinkedEventFailed:
		return TransferResultLinkedEventFailed
	case tb.TransferExceedsCredits:
		return TransferResultExceedsCredits
	case tb.TransferExceedsDebits:
		return TransferResultExceedsDebits
	case tb.TransferDebitAccountNotFound, tb.TransferCreditAccountNotFound:
		return TransferResultAccountNotFound
	case tb.TransferPendingTransferNotFound,
		tb.TransferPendingTransferNotPending,
		tb.TransferPendingTransferHasDifferentDebitAccountID,
		tb.TransferPendingTransferHasDifferentCreditAccountID,
		tb.TransferPendingTransferHasDifferentLedger,
		tb.TransferPendingTransferHasDifferentCode,
		tb.TransferPendingTransferHasDifferentAmount,
		tb.TransferExceedsPendingTransferAmount,
		tb.TransferPendingTransferAlreadyPosted,
		tb.TransferPendingTransferAlreadyVoided,
		tb.TransferPendingTransferExpired:
		return TransferResultPendingTransferNotFound
	default:
		return TransferResultInvalid
	}
}

func (c *RealClient) AccountBalance(_ context.Context, accountID string) (int64, bool, error) {
	id, err := idToUint128(accountID)
	if err != nil {
		return 0, false, err
	}
	accounts, err := c.tb.LookupAccounts([]tb.Uint128{id})
	if err != nil {
		return 0, false, fmt.Errorf("ledger: LookupAccounts: %w", err)
	}
	if len(accounts) == 0 {
		return 0, false, nil
	}
	a := accounts[0]
	creditsLo, creditsHi := a.CreditsPosted.Uint64()
	debitsLo, debitsHi := a.DebitsPosted.Uint64()
	if creditsHi != 0 || debitsHi != 0 {
		return 0, false, fmt.Errorf("ledger: account %q balance overflows int64", accountID)
	}
	return int64(creditsLo) - int64(debitsLo), true, nil
}
