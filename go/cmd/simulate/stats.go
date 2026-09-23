package main

import (
	"sort"
	"time"
)

// txResult is what one simulated transfer produced — a bare Transfer in
// -mode=transfer, or one wrapped in a single-child Transaction in
// -mode=transaction — the outcome it was steered toward, the label Go
// actually settled it under, and how long the whole exchange took. final is
// a human-readable state label for reporting only (a transactionpb.
// TransactionState's own String(), or for a bare Transfer the label
// fromTransferOutcome builds from transfer.OutcomeKind's own name); moved and
// open are what the rest of this package actually decides on. summarize
// turns a slice of these into a load report; computeExpected and reconcile
// use the completed ones to check the ledger.
type txResult struct {
	transactionID string // "" in -mode=transfer: no Transaction wraps it
	transferID    string // the leg: what advance watches, and settles once it stages
	fromWallet    string
	toWallet      string
	amountMinor   int64
	planned       outcome
	final         string
	moved         bool // this result actually moved its money — the only fact the balance reconciliation cares about
	open          bool // still not terminal when last checked — what awaitStuck keeps looking at
	settled       bool // planned has been acted on (or the leg resolved without it); only the outcome is left to see
	reason        string
	err           error
	latency       time.Duration
}

// completed reports whether this transaction actually moved its money. A
// rollback, an organic failure, or a transport/RPC error all mean no.
func (r txResult) completed() bool {
	return r.err == nil && r.moved
}

// stuck reports whether this transaction was still open when last checked —
// what awaitStuck keeps looking at.
func (r txResult) stuck() bool {
	return r.err == nil && r.open
}

// summary is the load report: how many transactions landed in each final
// state, how many were still open when the run ended, how many never got a
// state at all (a transport/RPC error), and latency across the run.
type summary struct {
	total      int
	byState    map[string]int
	openCount  int
	errors     int
	wallClock  time.Duration
	throughput float64 // completed RPC round trips per second, wall-clock
	p50        time.Duration
	p95        time.Duration
	p99        time.Duration
}

// summarize aggregates results, produced over wallClock of real time
// (elapsed while every worker ran, not summed per-transaction latency).
func summarize(results []txResult, wallClock time.Duration) summary {
	s := summary{total: len(results), byState: make(map[string]int), wallClock: wallClock}

	latencies := make([]time.Duration, 0, len(results))
	for _, r := range results {
		if r.err != nil {
			s.errors++
			s.byState["rpc_error"]++
			continue
		}
		s.byState[r.final]++
		if r.open {
			s.openCount++
		}
		latencies = append(latencies, r.latency)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	s.p50 = percentile(latencies, 0.50)
	s.p95 = percentile(latencies, 0.95)
	s.p99 = percentile(latencies, 0.99)
	if wallClock > 0 {
		s.throughput = float64(s.total) / wallClock.Seconds()
	}
	return s
}

// percentile returns the value at p (0..1) in sorted, ascending durations,
// by nearest rank — exact at p=0 and p=1, and needs no interpolation.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// computeExpected folds initial (each wallet's seeded starting balance) with
// the net effect of every result that actually completed, giving what each
// wallet's balance ought to be once the run is over. Anything that didn't
// complete — rolled back, organically failed, or errored — moved no money
// and contributes no delta.
func computeExpected(initial map[string]int64, results []txResult) map[string]int64 {
	expected := make(map[string]int64, len(initial))
	for walletID, amount := range initial {
		expected[walletID] = amount
	}
	for _, r := range results {
		if !r.completed() {
			continue
		}
		expected[r.fromWallet] -= r.amountMinor
		expected[r.toWallet] += r.amountMinor
	}
	return expected
}
