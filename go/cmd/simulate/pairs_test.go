package main

import (
	"math/rand/v2"
	"testing"
)

func TestPickPairAlwaysDistinctAndInRange(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(1, 1))
	for _, n := range []int{2, 3, 5, 50} {
		for i := 0; i < 2000; i++ {
			from, to := pickPair(r, n)
			if from == to {
				t.Fatalf("pickPair(n=%d) = (%d, %d), want distinct", n, from, to)
			}
			if from < 0 || from >= n || to < 0 || to >= n {
				t.Fatalf("pickPair(n=%d) = (%d, %d), want both in [0,%d)", n, from, to, n)
			}
		}
	}
}

func TestPickPairCoversEveryIndex(t *testing.T) {
	t.Parallel()

	const n = 5
	r := rand.New(rand.NewPCG(1, 1))
	seen := make(map[int]bool)
	for i := 0; i < 2000; i++ {
		from, to := pickPair(r, n)
		seen[from] = true
		seen[to] = true
	}
	if len(seen) != n {
		t.Errorf("saw %d distinct indices over 2000 draws, want all %d", len(seen), n)
	}
}
