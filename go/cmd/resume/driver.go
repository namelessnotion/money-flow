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

// drive redelivers t's trigger until the aggregate's own stream stops growing,
// and reports how many wake-ups that took. Redelivery is what at-least-once
// already permits (go/docs/adr/0001), so the last, inert one costs nothing.
func (d driver) drive(ctx context.Context, t target) (int, error) {
	trigger := saga.Trigger{AggregateType: t.aggregateType, AggregateID: t.aggregateID}

	for round := 1; round <= maxWakeUps; round++ {
		before, err := d.store.Load(ctx, t.aggregateType, t.aggregateID)
		if err != nil {
			return round, err
		}
		if err := d.orchestrator.Handle(ctx, trigger); err != nil {
			return round, err
		}
		after, err := d.store.Load(ctx, t.aggregateType, t.aggregateID)
		if err != nil {
			return round, err
		}
		if len(after) == len(before) {
			return round, nil
		}
	}
	return maxWakeUps, fmt.Errorf("still moving after %d wake-ups; look at it rather than driving it further", maxWakeUps)
}
