package main

import (
	"math/rand/v2"
	"testing"
)

func TestPickOutcomeAlwaysCompletesAtZeroRate(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(1, 1))
	for i := 0; i < 1000; i++ {
		if got := pickOutcome(r, 0); got != outcomeComplete {
			t.Fatalf("pickOutcome(rate=0) = %v, want %v", got, outcomeComplete)
		}
	}
}

func TestPickOutcomeAlwaysRollsBackAtOneRate(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(1, 1))
	for i := 0; i < 1000; i++ {
		if got := pickOutcome(r, 1); got != outcomeRollback {
			t.Fatalf("pickOutcome(rate=1) = %v, want %v", got, outcomeRollback)
		}
	}
}

// A rate strictly between 0 and 1 must actually produce both outcomes over
// enough trials, and land roughly on the requested split — not exactly
// (it's still a coin flip), but nowhere near uniform-by-accident.
func TestPickOutcomeRespectsAMidRange(t *testing.T) {
	t.Parallel()

	const trials = 10_000
	r := rand.New(rand.NewPCG(1, 1))
	rollbacks := 0
	for i := 0; i < trials; i++ {
		if pickOutcome(r, 0.3) == outcomeRollback {
			rollbacks++
		}
	}

	got := float64(rollbacks) / trials
	if got < 0.25 || got > 0.35 {
		t.Errorf("rollback fraction = %.3f, want close to 0.3", got)
	}
}
