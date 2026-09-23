package main

import (
	"context"
	"fmt"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// maxWakeUps bounds one aggregate's driving. A saga that keeps moving past this
// is not something to keep poking: dispatch is sliced (see
// transaction.maxDispatchPerStep) so a wide Transaction legitimately takes
// several wake-ups, but a great many means something is not converging and an
// operator should look rather than let this loop.
const maxWakeUps = 100

// driver drives aggregates through the real orchestrator, so a Transfer wakes
// the Transaction that owns it exactly as a delivered trigger would.
type driver struct {
	orchestrator *saga.Orchestrator
	store        eventstore.Store
}

// inFlight reports whether t still has anywhere to go — the filter behind
// -open. It is here rather than in the SQL because the answer is a fold of the
// aggregate's own stream, which is the aggregates' business and not the
// database's.
func (d driver) inFlight(ctx context.Context, t target) (bool, error) {
	switch t.aggregateType {
	case transaction.AggregateType:
		return transaction.IsOpen(ctx, d.store, t.aggregateID)
	case transfer.AggregateType:
		outcome, err := transfer.Outcome(ctx, d.store, t.aggregateID)
		if err != nil {
			return false, err
		}
		return outcome == transfer.OutcomeInFlight, nil
	default:
		return false, fmt.Errorf("no driver for aggregate type %q", t.aggregateType)
	}
}

// drive wakes t, and everything t's progress depends on, until none of it
// grows any more, and reports how many rounds that took. Redelivery is what
// at-least-once already permits (go/docs/adr/0001), so the last, inert round
// costs nothing.
//
// Waking t alone is not enough. Driving a Transaction requests its children,
// and the trigger that would run each one is exactly the kind this tool exists
// because nobody received; without waking them here too, every hop of the DAG
// would cost another run.
func (d driver) drive(ctx context.Context, t target) (int, error) {
	for round := 1; round <= maxWakeUps; round++ {
		scope, err := d.scope(ctx, t)
		if err != nil {
			return round, err
		}
		before, err := d.footprint(ctx, scope)
		if err != nil {
			return round, err
		}
		for _, s := range scope {
			if err := d.orchestrator.Handle(ctx, saga.Trigger{AggregateType: s.aggregateType, AggregateID: s.aggregateID}); err != nil {
				return round, fmt.Errorf("%s: %w", s, err)
			}
		}

		// Re-derived rather than reused: this round may have requested
		// children that did not exist when it began.
		if scope, err = d.scope(ctx, t); err != nil {
			return round, err
		}
		after, err := d.footprint(ctx, scope)
		if err != nil {
			return round, err
		}
		if after == before {
			return round, nil
		}
	}
	return maxWakeUps, fmt.Errorf("still moving after %d wake-ups; look at it rather than driving it further", maxWakeUps)
}

// scope is every aggregate one round of driving t wakes: t itself and, when t
// is or belongs to a Transaction, that Transaction and every Transfer it has
// requested. Driving a child Transfer is driving its Transaction's progress,
// so the Transaction moving counts as t advancing.
func (d driver) scope(ctx context.Context, t target) ([]target, error) {
	transactionID := ""
	switch t.aggregateType {
	case transaction.AggregateType:
		transactionID = t.aggregateID
	case transfer.AggregateType:
		owner, err := transfer.OwningTransaction(ctx, d.store, t.aggregateID)
		if err != nil {
			return nil, err
		}
		transactionID = owner
	}

	scope := []target{t}
	if transactionID == "" {
		return scope, nil
	}
	if transactionID != t.aggregateID {
		scope = append(scope, target{aggregateType: transaction.AggregateType, aggregateID: transactionID})
	}
	children, err := transaction.ChildTransferIDs(ctx, d.store, transactionID)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		if child != t.aggregateID {
			scope = append(scope, target{aggregateType: transfer.AggregateType, aggregateID: child})
		}
	}
	return scope, nil
}

// footprint totals every stream in scope: the measure of whether a round moved
// anything at all.
func (d driver) footprint(ctx context.Context, scope []target) (int, error) {
	total := 0
	for _, s := range scope {
		events, err := d.store.Load(ctx, s.aggregateType, s.aggregateID)
		if err != nil {
			return 0, err
		}
		total += len(events)
	}
	return total, nil
}
