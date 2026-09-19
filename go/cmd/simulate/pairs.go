package main

import "math/rand/v2"

// pickPair returns two distinct indices in [0,n) — the sender and receiver
// for one simulated transfer. Picking to from the n-1 slots that aren't
// from, then shifting it past from, gives a distinct pair in one draw each,
// instead of drawing two indices and rerolling on collision.
//
// Requires n >= 2; callers must not ask for a pair from fewer entities than
// that.
func pickPair(r *rand.Rand, n int) (from, to int) {
	from = r.IntN(n)
	to = r.IntN(n - 1)
	if to >= from {
		to++
	}
	return from, to
}
