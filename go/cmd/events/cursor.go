package main

import (
	"slices"
	"time"
)

// maxTrackedGap bounds how many missing sequence numbers one read may open.
// Concurrent writers leave gaps of a handful; anything larger is identity
// values burnt by rolled-back inserts, not events still on their way.
const maxTrackedGap = 1000

// cursor is how far through the global log the tail has read.
//
// global_seq is an identity column: a writer takes its value at INSERT but the
// row only becomes visible at COMMIT, so a slow writer can commit seq 12 after
// seq 13 has already been read. Reading strictly "after the highest seen"
// would skip 12 forever. The cursor keeps each gap it passes as a hole to ask
// for again, and gives up on a hole after the grace period, since a
// rolled-back insert burns its value for good.
type cursor struct {
	after int64
	holes map[int64]time.Time // opened at
	grace time.Duration
}

func newCursor(after int64, grace time.Duration) *cursor {
	return &cursor{after: after, holes: map[int64]time.Time{}, grace: grace}
}

// position is what to read next: everything after `after`, plus the holes.
func (c *cursor) position() (after int64, holes []int64) {
	holes = make([]int64, 0, len(c.holes))
	for seq := range c.holes {
		holes = append(holes, seq)
	}
	slices.Sort(holes)
	return c.after, holes
}

// advance records the sequence numbers a read returned, in ascending order.
func (c *cursor) advance(seqs []int64, now time.Time) {
	for _, seq := range seqs {
		if _, ok := c.holes[seq]; ok {
			delete(c.holes, seq)
			continue
		}
		if seq <= c.after {
			continue
		}
		if seq-c.after-1 <= maxTrackedGap {
			for missing := c.after + 1; missing < seq; missing++ {
				c.holes[missing] = now
			}
		}
		c.after = seq
	}
	for seq, opened := range c.holes {
		if now.Sub(opened) > c.grace {
			delete(c.holes, seq)
		}
	}
}
