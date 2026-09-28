package saga

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

type attemptKey struct{}

// ctxRecorder is a slog handler that remembers, for each record, which
// attempt the context it was logged with belonged to.
type ctxRecorder struct {
	mu      sync.Mutex
	records map[string][]int
}

func (r *ctxRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *ctxRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *ctxRecorder) WithGroup(string) slog.Handler            { return r }
func (r *ctxRecorder) Handle(ctx context.Context, rec slog.Record) error {
	attempt, _ := ctx.Value(attemptKey{}).(int)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[rec.Message] = append(r.records[rec.Message], attempt)
	return nil
}

func (r *ctxRecorder) attemptsLoggedFor(msg string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records[msg]
}

// markAttempts is an observer that tags each attempt's context with its
// number and remembers each attempt's outcome.
func markAttempts(outcomes *[]error, mu *sync.Mutex) AttemptObserver {
	return func(ctx context.Context, _ Trigger, attempt int) (context.Context, func(error)) {
		return context.WithValue(ctx, attemptKey{}, attempt), func(err error) {
			mu.Lock()
			defer mu.Unlock()
			*outcomes = append(*outcomes, err)
		}
	}
}

// Each attempt runs in the context its observer opened, and the lines about
// that attempt are logged with it, so a log line can carry the trace of the
// attempt it describes: "retrying" names the attempt that just failed,
// "handled" the one that succeeded.
func TestConsumer_LogsEachAttemptInTheContextItsObserverOpened(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{message(0, observedKey, observedValue)}}
	var (
		mu       sync.Mutex
		outcomes []error
		seen     []int
	)
	logs := &ctxRecorder{records: map[string][]int{}}
	calls := 0
	c := NewConsumer(reader, handlerFuncCtx(func(ctx context.Context, _ Trigger) error {
		attempt, _ := ctx.Value(attemptKey{}).(int)
		seen = append(seen, attempt)
		calls++
		if calls < 2 {
			return errors.New("store briefly unreachable")
		}
		return nil
	}), WithAttempts(3), WithBackoff(0), WithLogger(slog.New(logs)), WithAttemptObserver(markAttempts(&outcomes, &mu)))

	if err := runUntilIdle(t, c); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("handler ran in attempts %v, want [1 2]", seen)
	}
	if got := logs.attemptsLoggedFor("saga: retrying"); len(got) != 1 || got[0] != 1 {
		t.Errorf("retrying logged in attempts %v, want [1], the attempt that failed", got)
	}
	if got := logs.attemptsLoggedFor("saga: handled"); len(got) != 1 || got[0] != 2 {
		t.Errorf("handled logged in attempts %v, want [2]", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(outcomes) != 2 || outcomes[0] == nil || outcomes[1] != nil {
		t.Errorf("observed outcomes %v, want a failure then a success", outcomes)
	}
}

// The halt is logged in the context of the last attempt, so the halt line
// leads straight to the trace of the attempt that failed for good.
func TestConsumer_LogsTheHaltInTheLastAttemptsContext(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{message(0, observedKey, observedValue)}}
	var (
		mu       sync.Mutex
		outcomes []error
	)
	logs := &ctxRecorder{records: map[string][]int{}}
	c := NewConsumer(reader, handlerFunc(func(Trigger) error { return errors.New("invariant broken") }),
		WithAttempts(2), WithBackoff(0), WithLogger(slog.New(logs)), WithAttemptObserver(markAttempts(&outcomes, &mu)))

	var halt *HaltError
	if err := runUntilIdle(t, c); !errors.As(err, &halt) {
		t.Fatalf("Run() error = %v, want a *HaltError", err)
	}
	if got := logs.attemptsLoggedFor("saga: HALTED; nothing further will be consumed from this reader"); len(got) != 1 || got[0] != 2 {
		t.Errorf("halt logged in attempts %v, want [2]", got)
	}
}

type handlerFuncCtx func(context.Context, Trigger) error

func (f handlerFuncCtx) Handle(ctx context.Context, t Trigger) error { return f(ctx, t) }
