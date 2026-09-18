package ledger

import "fmt"

// ledgerIDs maps a currency code to TigerBeetle's numeric `ledger` field —
// the namespace that partitions accounts so a transfer can only ever move
// value between two accounts on the same ledger. Placeholder for a
// single-currency v1; extend (or replace with a real registry) once
// multi-currency Transfers are a real requirement.
var ledgerIDs = map[string]uint32{
	"USD": 1,
}

// code is the single TigerBeetle transfer/account `code` (business
// categorization) this domain uses today. Placeholder, like ledgerIDs,
// until code-level segmentation is a real requirement.
const code = 1

// LedgerID resolves a currency code to TigerBeetle's numeric ledger id.
func LedgerID(currency string) (uint32, error) {
	id, ok := ledgerIDs[currency]
	if !ok {
		return 0, fmt.Errorf("ledger: unknown currency %q", currency)
	}
	return id, nil
}

// CurrencyOf is LedgerID's inverse: the currency code a TigerBeetle ledger id
// stands for.
func CurrencyOf(ledgerID uint32) (string, error) {
	for currency, id := range ledgerIDs {
		if id == ledgerID {
			return currency, nil
		}
	}
	return "", fmt.Errorf("ledger: unknown ledger id %d", ledgerID)
}
