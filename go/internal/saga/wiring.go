package saga

import (
	"context"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// Topic is where events for aggregateType are published.
//
// It mirrors the connector's route.topic.replacement of "${routedByValue}-events"
// (docker/cdc/events-connector.json), which routes by the aggregate_type
// column. Derived rather than listed so the two topic names have one owner
// between the publisher's configuration and every consumer of it.
func Topic(aggregateType string) string { return aggregateType + "-events" }

// Servers are the Transfer and Transaction services, tied together the one way
// they can be: transfer needs transaction's IsOpen and Exists checkers to
// authorize mint_source and to keep a Token reserved by an open Transaction out
// of another one's reach, and transaction needs the transfer server to dispatch
// its children through.
//
// The knot has to be tied identically wherever either service is constructed,
// or the same request behaves differently depending on which binary answered
// it. It is tied here, in the package that exists to drive both, rather than
// separately in each command.
type Servers struct {
	Transfer    *transfer.Server
	Transaction *transaction.Server

	store eventstore.Store
}

// Wire constructs both services over one store and ledger.
func Wire(store eventstore.Store, lc ledger.Client) Servers {
	// transfer is built first and given transaction's IsOpen/Exists as plain
	// functions, not methods on a transaction.Server: that keeps transfer from
	// ever importing transaction, and means neither has to exist before the
	// other.
	isOpen := func(ctx context.Context, transactionID string) (bool, error) {
		return transaction.IsOpen(ctx, store, transactionID)
	}
	exists := func(ctx context.Context, transactionID string) (bool, error) {
		return transaction.Exists(ctx, store, transactionID)
	}
	transfers := transfer.NewServer(store, lc, isOpen, exists)

	return Servers{Transfer: transfers, Transaction: transaction.NewServer(store, transfers), store: store}
}

// Orchestrator returns the orchestrator that drives these services from
// delivered triggers.
func (s Servers) Orchestrator() *Orchestrator {
	owner := func(ctx context.Context, transferID string) (string, error) {
		return transfer.OwningTransaction(ctx, s.store, transferID)
	}
	return New(s.Transfer, s.Transaction, owner)
}
