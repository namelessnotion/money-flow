package main

import (
	"context"
	"time"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// Since the async cutover (go/docs/adr/0006) every call this tool makes
// records a decision and returns, and go/cmd/orchestrator acts on it later.
// So between making a call and acting on what it caused there is always a
// wait, and this file is the tool's whole vocabulary for it.

// legReader reports where one Transfer leg actually stands.
//
// No RPC answers that. GetTransactionState reports only the Transaction's own
// state, which is STARTED whether its leg has merely been accepted or has
// already staged, and TransferService has no read at all. Settling a leg
// before it stages is refused, and the refusal is written to the Transfer's
// stream, so the tool reads the one place that knows: the event log — the same
// log its post-run verification reads.
type legReader func(ctx context.Context, transferID string) (transfer.OutcomeKind, error)

func storeLegReader(store eventstore.Store) legReader {
	return func(ctx context.Context, transferID string) (transfer.OutcomeKind, error) {
		return transfer.Outcome(ctx, store, transferID)
	}
}

// settleWait is how long the tool keeps looking at something go/cmd/orchestrator
// has yet to reach: attempts more looks after the first, delay apart.
type settleWait struct {
	attempts int
	delay    time.Duration
}

// pause waits one delay, and reports false if ctx ended first.
func (w settleWait) pause(ctx context.Context) bool {
	if w.delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(w.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// advanceFunc takes one look at r and moves it on as far as that look
// allows — settling its leg if the leg has just staged, and recording where
// it stands either way. It never waits; awaitResult and awaitStuck do that.
type advanceFunc func(ctx context.Context, r *txResult)

// awaitResult looks at r until it closes or the wait runs out.
func awaitResult(ctx context.Context, r *txResult, wait settleWait, advance advanceFunc) {
	advance(ctx, r)
	for attempt := 0; attempt < wait.attempts && r.stuck(); attempt++ {
		if !wait.pause(ctx) {
			return
		}
		advance(ctx, r)
	}
}

// awaitStuck is the catch-up pass after the load: every result still open
// gets up to wait.attempts more looks, a round of them each delay. Each look
// carries on from where that result left off, so a leg settled during the
// load is not settled again. Mutates results in place and returns how many
// are still open.
func awaitStuck(ctx context.Context, results []txResult, wait settleWait, advance advanceFunc) int {
	for attempt := 0; attempt < wait.attempts; attempt++ {
		idx := stuckIndices(results)
		if len(idx) == 0 {
			return 0
		}
		if !wait.pause(ctx) {
			break
		}
		for _, i := range idx {
			start := time.Now()
			advance(ctx, &results[i])
			results[i].latency += time.Since(start)
		}
	}
	return len(stuckIndices(results))
}

func stuckIndices(results []txResult) []int {
	var idx []int
	for i, r := range results {
		if r.stuck() {
			idx = append(idx, i)
		}
	}
	return idx
}
