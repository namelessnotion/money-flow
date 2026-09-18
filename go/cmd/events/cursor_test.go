package main

import (
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func TestCursorAdvancesPastContiguousEvents(t *testing.T) {
	c := newCursor(10, 5*time.Second)

	c.advance([]int64{11, 12, 13}, t0)

	after, holes := c.position()
	if after != 13 || len(holes) != 0 {
		t.Fatalf("position = (%d, %v), want (13, [])", after, holes)
	}
}

// An identity value is taken at insert but only becomes visible at commit, so
// a writer that commits late leaves a gap behind events already seen.
func TestCursorAsksAgainForAGapUntilItFills(t *testing.T) {
	c := newCursor(10, 5*time.Second)

	c.advance([]int64{11, 14}, t0)
	if _, holes := c.position(); !slices.Equal(holes, []int64{12, 13}) {
		t.Fatalf("holes = %v, want [12 13]", holes)
	}

	c.advance([]int64{13}, t0.Add(time.Second))
	if after, holes := c.position(); after != 14 || !slices.Equal(holes, []int64{12}) {
		t.Fatalf("position = (%d, %v), want (14, [12])", after, holes)
	}
}

// A rolled-back insert burns its identity value for good, so a gap is given up
// on once it has been open longer than any commit plausibly takes.
func TestCursorGivesUpOnAGapAfterTheGracePeriod(t *testing.T) {
	c := newCursor(10, 5*time.Second)

	c.advance([]int64{12}, t0)
	c.advance(nil, t0.Add(4*time.Second))
	if _, holes := c.position(); !slices.Equal(holes, []int64{11}) {
		t.Fatalf("holes = %v before the grace period, want [11]", holes)
	}

	c.advance(nil, t0.Add(6*time.Second))
	if _, holes := c.position(); len(holes) != 0 {
		t.Fatalf("holes = %v after the grace period, want none", holes)
	}
}

func TestCursorDoesNotTrackAnImplausiblyLargeGap(t *testing.T) {
	c := newCursor(0, 5*time.Second)

	c.advance([]int64{maxTrackedGap + 10}, t0)

	if _, holes := c.position(); len(holes) != 0 {
		t.Fatalf("tracked %d holes, want none", len(holes))
	}
}
