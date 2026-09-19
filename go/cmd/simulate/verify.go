package main

import (
	"context"
	"fmt"

	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

// tokenAggregateType mirrors token.AggregateType. Duplicated rather than
// imported: internal/token pulls in internal/ledger for its TigerBeetle
// client, and this tool has no other reason to link that in — it only ever
// reads the Postgres event log, never TigerBeetle itself. Importing token
// here would force every build of this tool to link tigerbeetle-go's cgo
// bindings, native library and all, for one string constant.
const tokenAggregateType = "token"

// lastRecordedBalance returns the PostedMinorUnits off the most recent
// TokenBalanceRecorded in events (oldest first, as Store.Load returns them)
// — the same "last one on the stream wins" rule token.RecordBalances itself
// relies on when deciding whether a balance changed. found is false when
// the token has never had a balance published yet.
func lastRecordedBalance(events []eventstore.Event) (minorUnits int64, found bool, err error) {
	for _, e := range events {
		if e.EventType != eventstore.EventType(&tokenpb.TokenBalanceRecorded{}) {
			continue
		}
		msg, decodeErr := e.Decode()
		if decodeErr != nil {
			return 0, false, decodeErr
		}
		rec, ok := msg.(*tokenpb.TokenBalanceRecorded)
		if !ok {
			continue
		}
		minorUnits, found = rec.GetPostedMinorUnits(), true
	}
	return minorUnits, found, nil
}

// walletBalance is walletID's current ledger balance: the sum of the last
// recorded posted balance over every Token ever minted for it, matching
// token.proto's own statement of the rule ("An Account's balance is the sum
// over its Wallet's Tokens"). A Token minted but never yet touched by a
// ledger write (no TokenBalanceRecorded published for it) contributes zero.
func walletBalance(ctx context.Context, store eventstore.Store, walletID string) (int64, error) {
	tokenIDs, err := wallet.TokensOf(ctx, store, walletID)
	if err != nil {
		return 0, fmt.Errorf("wallet %q: %w", walletID, err)
	}

	var total int64
	for _, tokenID := range tokenIDs {
		events, err := store.Load(ctx, tokenAggregateType, tokenID)
		if err != nil {
			return 0, fmt.Errorf("load token %q: %w", tokenID, err)
		}
		balance, _, err := lastRecordedBalance(events)
		if err != nil {
			return 0, fmt.Errorf("token %q: %w", tokenID, err)
		}
		total += balance
	}
	return total, nil
}

// reconciliation is one entity's expected-vs-actual balance after a run.
type reconciliation struct {
	name     string
	walletID string
	expected int64
	actual   int64
}

func (r reconciliation) ok() bool { return r.expected == r.actual }

// reconcile compares every entity's actual ledger balance, read from the
// event log, against expected — its seeded starting balance plus the net of
// every transaction the run recorded as actually completed (see
// computeExpected).
func reconcile(ctx context.Context, store eventstore.Store, entities []entity, expected map[string]int64) ([]reconciliation, error) {
	out := make([]reconciliation, 0, len(entities))
	for _, e := range entities {
		actual, err := walletBalance(ctx, store, e.walletID)
		if err != nil {
			return nil, fmt.Errorf("entity %q: %w", e.name, err)
		}
		out = append(out, reconciliation{name: e.name, walletID: e.walletID, expected: expected[e.walletID], actual: actual})
	}
	return out, nil
}
