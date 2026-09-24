package contention

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The window doubles from the base up to the cap, and every wait falls
// inside the window for its attempt: full jitter, never zero-width, never
// longer than one collision's worth of work.
func TestBackoff_StaysInsideAWindowThatDoublesUpToTheCap(t *testing.T) {
	t.Parallel()
	for attempt, window := range map[int]time.Duration{
		0:  time.Millisecond,
		1:  2 * time.Millisecond,
		5:  32 * time.Millisecond,
		6:  maxWindow,
		63: maxWindow,
	} {
		if got := windowFor(attempt); got != window {
			t.Errorf("windowFor(%d) = %v, want %v", attempt, got, window)
		}
		for range 1000 {
			if d := backoff(attempt); d < 0 || d >= window {
				t.Fatalf("backoff(%d) = %v, want within [0, %v)", attempt, d, window)
			}
		}
	}
}

// A loop with no count bound relies on its context to end it: Wait must not
// hold up a shutdown for the rest of its window.
func TestWait_ReturnsWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, 63); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
}

func TestWait_ReturnsNilOnceItHasWaited(t *testing.T) {
	t.Parallel()
	if err := Wait(context.Background(), 0); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}
