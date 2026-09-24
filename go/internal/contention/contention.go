// Package contention is how a loop that lost an optimistic-concurrency race
// to a write that really landed waits before trying again
// (go/docs/adr/0003, amended 2026-09-24).
//
// Such a loop has no count bound: every lap is paid for by someone else's
// progress, so a hot stream drains instead of halting the orchestrator. What
// keeps it from spinning is the caller's own check that the stream it lost
// actually moved, and its context. Wait supplies the rest: a pause that
// decorrelates contenders that just collided, so they don't all try again
// into the same collision.
package contention

import (
	"context"
	"math/rand/v2"
	"time"
)

// The wait is a random slice of a window that doubles from minWindow up to
// maxWindow. It is there to spread out contenders, not to outlast a fault, so
// its scale is one attempt (a few store round trips), not the orchestrator's
// retry backoff.
const (
	minWindow = time.Millisecond
	maxWindow = 50 * time.Millisecond
)

// Wait pauses before a contended loop's next attempt, attempt counting from
// 0 for the first retry. It returns ctx's error if ctx ends first, which is
// what ends a loop that has no count bound.
func Wait(ctx context.Context, attempt int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(backoff(attempt))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// backoff is a uniformly random duration inside attempt's window.
func backoff(attempt int) time.Duration {
	return rand.N(windowFor(attempt))
}

// windowFor is minWindow doubled attempt times, capped at maxWindow.
func windowFor(attempt int) time.Duration {
	if attempt >= 16 {
		return maxWindow
	}
	return min(maxWindow, minWindow<<attempt)
}
