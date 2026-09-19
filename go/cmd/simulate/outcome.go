package main

import "math/rand/v2"

// outcome is the terminal state one simulated transaction is steered toward
// once its Transfer leg has staged: settled to completion, or deliberately
// rolled back — mirroring the two ways a real staged Transfer (an ACH entry,
// say) resolves once it is out of the platform's own hands (settle/return).
type outcome int

const (
	outcomeComplete outcome = iota
	outcomeRollback
)

func (o outcome) String() string {
	if o == outcomeRollback {
		return "rollback"
	}
	return "complete"
}

// pickOutcome decides which way one simulated transaction should be
// steered, weighted by rollbackRate (0: always complete, 1: always roll
// back). The decision is made up front, independent of either wallet's
// actual balance — an organic insufficient-funds failure can still preempt
// it once the Transfer is actually requested, and that's a real outcome
// worth keeping, not a bug in this weighting.
func pickOutcome(r *rand.Rand, rollbackRate float64) outcome {
	if r.Float64() < rollbackRate {
		return outcomeRollback
	}
	return outcomeComplete
}
