package contention

import (
	"context"
	"errors"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

// A lap lost to contention is invisible in the saga's outcome, which converges
// either way, so each wait is marked on whatever span the loop runs in. A hot
// Wallet's re-plan laps then show up in the trace of the step that paid for
// them.
func TestWait_MarksTheWaitOnTheActiveSpan(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	ctx, span := tp.Tracer("test").Start(context.Background(), "prepare")

	if err := Wait(ctx, 2); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	span.End()

	events := spans.Ended()[0].Events()
	if len(events) != 1 || events[0].Name != "contention.wait" {
		t.Fatalf("events = %v, want one contention.wait", events)
	}
	var attempt, waited bool
	for _, kv := range events[0].Attributes {
		switch kv.Key {
		case "money_flow.contention.attempt":
			attempt = kv.Value.AsInt64() == 2
		case "money_flow.contention.wait_ms":
			waited = kv.Value.AsFloat64() >= 0 && kv.Value.AsFloat64() <= float64(maxWindow.Milliseconds())
		}
	}
	if !attempt || !waited {
		t.Errorf("event attributes = %v, want attempt 2 and a wait inside the window", events[0].Attributes)
	}
}
