package saga

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"testing"
	"time"
)

// scriptedReader delivers a fixed list of messages and records what was
// committed. Once the list runs out it blocks on ctx, the way a real reader
// waits on an idle topic.
type scriptedReader struct {
	messages  []Message
	next      int
	committed []Message
	commitErr error
}

func (r *scriptedReader) Fetch(ctx context.Context) (Message, error) {
	if r.next >= len(r.messages) {
		<-ctx.Done()
		return Message{}, ctx.Err()
	}
	m := r.messages[r.next]
	r.next++
	return m, nil
}

func (r *scriptedReader) Commit(_ context.Context, m Message) error {
	if r.commitErr != nil {
		return r.commitErr
	}
	r.committed = append(r.committed, m)
	return nil
}

// handlerFunc adapts a plain function to Handler.
type handlerFunc func(Trigger) error

func (f handlerFunc) Handle(_ context.Context, t Trigger) error { return f(t) }

func message(offset int64, key, value string) Message {
	return Message{Topic: "transfer-events", Partition: 0, Offset: offset, Key: []byte(key), Value: []byte(value)}
}

func quietConsumer(reader Reader, handler Handler, opts ...ConsumerOption) *Consumer {
	opts = append([]ConsumerOption{
		WithLogger(log.New(io.Discard, "", 0)),
		WithBackoff(time.Millisecond),
	}, opts...)
	return NewConsumer(reader, handler, opts...)
}

// A short-lived context stands in for shutdown: the loop drains what is there
// and then returns cleanly rather than treating cancellation as a failure.
func runUntilIdle(t *testing.T, c *Consumer) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	return c.Run(ctx)
}

func TestConsumer_HandlesThenCommitsEachMessage(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{
		message(0, observedKey, observedValue),
		message(1, observedKey, observedValue),
	}}
	var handled []string
	c := quietConsumer(reader, handlerFunc(func(tr Trigger) error {
		handled = append(handled, tr.AggregateID)
		return nil
	}))

	if err := runUntilIdle(t, c); err != nil {
		t.Fatalf("Run() error = %v, want nil on shutdown", err)
	}
	if len(handled) != 2 {
		t.Errorf("handled %d messages, want 2", len(handled))
	}
	if len(reader.committed) != 2 || reader.committed[1].Offset != 1 {
		t.Errorf("committed = %v, want both offsets", reader.committed)
	}
}

// The offset must move only after the side effects, so a crash redelivers
// rather than skips. Nothing else in the design makes at-least-once true.
func TestConsumer_CommitsOnlyAfterTheHandlerHasRun(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{message(0, observedKey, observedValue)}}
	committedDuringHandle := -1
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		committedDuringHandle = len(reader.committed)
		return nil
	}))

	if err := runUntilIdle(t, c); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if committedDuringHandle != 0 {
		t.Errorf("%d message(s) already committed when the handler ran, want 0", committedDuringHandle)
	}
	if len(reader.committed) != 1 {
		t.Errorf("committed %d messages after the handler returned, want 1", len(reader.committed))
	}
}

// A trigger re-folds authoritative state, so a failure that clears on its own
// costs a retry and nothing else.
func TestConsumer_RetriesAFailingHandlerAndCarriesOn(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{message(0, observedKey, observedValue)}}
	calls := 0
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		calls++
		if calls < 3 {
			return errors.New("store briefly unreachable")
		}
		return nil
	}), WithAttempts(3))

	if err := runUntilIdle(t, c); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls != 3 {
		t.Errorf("handler called %d times, want 3", calls)
	}
	if len(reader.committed) != 1 {
		t.Errorf("committed %d messages, want 1 once the retry succeeded", len(reader.committed))
	}
}

// go/docs/adr/0003: a message that will not process stops the consumer. It is
// never committed and never stepped over, so the next start resumes on it.
func TestConsumer_HaltsOnAPersistentlyFailingMessage(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{
		message(7, observedKey, observedValue),
		message(8, observedKey, observedValue),
	}}
	boom := errors.New("invariant broken")
	calls := 0
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		calls++
		return boom
	}), WithAttempts(2))

	err := runUntilIdle(t, c)

	var halt *HaltError
	if !errors.As(err, &halt) {
		t.Fatalf("Run() error = %v, want a *HaltError", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("halt does not wrap the cause: %v", err)
	}
	if halt.Message.Offset != 7 {
		t.Errorf("halted on offset %d, want the failing 7", halt.Message.Offset)
	}
	if calls != 2 {
		t.Errorf("handler called %d times, want the 2 attempts allowed", calls)
	}
	if len(reader.committed) != 0 {
		t.Errorf("committed %v; a halted message must never be committed", reader.committed)
	}
	if reader.next != 1 {
		t.Errorf("read %d messages, want 1: the consumer must not move past the failure", reader.next)
	}
}

// A malformed envelope will never parse, so retrying it is theatre. It still
// halts — the message is not skipped — but it does so immediately.
func TestConsumer_HaltsImmediatelyOnAMessageThatCannotParse(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{message(3, observedKey, "not json at all")}}
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		t.Error("handler ran for a message that could not be parsed")
		return nil
	}))

	err := runUntilIdle(t, c)

	var halt *HaltError
	if !errors.As(err, &halt) {
		t.Fatalf("Run() error = %v, want a *HaltError", err)
	}
	if halt.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0: an unparseable message is not retried", halt.Attempts)
	}
	if len(reader.committed) != 0 {
		t.Errorf("committed %v, want nothing", reader.committed)
	}
}

// Losing the ability to record progress is not something to consume through:
// it would replay indefinitely on every restart.
func TestConsumer_StopsWhenItCannotCommit(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{
		messages:  []Message{message(0, observedKey, observedValue)},
		commitErr: errors.New("broker gone"),
	}
	c := quietConsumer(reader, handlerFunc(func(Trigger) error { return nil }))

	if err := runUntilIdle(t, c); err == nil {
		t.Error("Run() = nil, want the commit failure surfaced")
	}
}

// Shutdown is not a failure, and must not be reported as a halt.
func TestConsumer_ReturnsCleanlyOnShutdown(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := quietConsumer(reader, handlerFunc(func(Trigger) error { return nil })).Run(ctx); err != nil {
		t.Errorf("Run() error = %v, want nil for a cancelled context", err)
	}
}

// A handler interrupted mid-retry by shutdown must not be mistaken for one
// that exhausted its attempts.
func TestConsumer_DoesNotHaltWhenShutdownInterruptsARetry(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{message(0, observedKey, observedValue)}}
	ctx, cancel := context.WithCancel(context.Background())
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		cancel()
		return errors.New("failing as we shut down")
	}), WithAttempts(5), WithBackoff(time.Hour))

	if err := c.Run(ctx); err != nil {
		t.Errorf("Run() error = %v, want nil: shutdown is not a halt", err)
	}
}

// A deadline that expires *below* the handler — a per-message timeout, a
// database connect timeout — is a dependency failing, not this process being
// asked to stop. Treating it as shutdown would return nil from Run, and
// cmd/orchestrator reads nil as "this consumer finished cleanly": it records
// no error and cancels nothing, so the sibling topic keeps running, the
// process stays up, and one topic is silently no longer consumed. That is the
// loss go/docs/adr/0003 halts to avoid, dressed up as health.
func TestConsumer_HaltsWhenADeadlineExpiresBelowTheHandler(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{message(9, observedKey, observedValue)}}
	// Wrapped, the way a real caller returns it: errors.Is sees straight
	// through twirp.InternalErrorWith and every other Unwrap-preserving error.
	timedOut := fmt.Errorf("ledger call: %w", context.DeadlineExceeded)
	calls := 0
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		calls++
		return timedOut
	}), WithAttempts(2))

	// Not runUntilIdle: the consumer's own context must stay live, or the
	// deadline under test is indistinguishable from this process stopping.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := c.Run(ctx)

	var halt *HaltError
	if !errors.As(err, &halt) {
		t.Fatalf("Run() error = %v, want a *HaltError", err)
	}
	if halt.Message.Offset != 9 {
		t.Errorf("halted on offset %d, want the failing 9", halt.Message.Offset)
	}
	if calls != 2 {
		t.Errorf("handler called %d times, want the 2 attempts allowed: a deadline below the handler is retryable", calls)
	}
	if len(reader.committed) != 0 {
		t.Errorf("committed %v; a halted message must never be committed", reader.committed)
	}
}

// context.Canceled arriving from a source other than this consumer's own ctx
// — a handler's own child context, a fetch or commit's internal cancellation
// — is a dependency failing, not this process being asked to stop. Treating
// it as shutdown would return nil from Run and, like a deadline arriving from
// below, read as a clean finish to cmd/orchestrator: no error recorded, the
// sibling consumer never cancelled, one topic silently no longer consumed.
func TestConsumer_HaltsOnACanceledContextThatIsNotItsOwn(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{message(11, observedKey, observedValue)}}
	// Wrapped, the way a real caller returns it: errors.Is sees straight
	// through any Unwrap-preserving error, including this consumer's own ctx
	// being live throughout.
	foreignCancel := fmt.Errorf("dependent call: %w", context.Canceled)
	calls := 0
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		calls++
		return foreignCancel
	}), WithAttempts(2))

	// Not runUntilIdle: the consumer's own context must stay live, or the
	// foreign cancellation under test is indistinguishable from this process
	// stopping.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := c.Run(ctx)

	var halt *HaltError
	if !errors.As(err, &halt) {
		t.Fatalf("Run() error = %v, want a *HaltError", err)
	}
	if halt.Message.Offset != 11 {
		t.Errorf("halted on offset %d, want the failing 11", halt.Message.Offset)
	}
	if calls != 2 {
		t.Errorf("handler called %d times, want the 2 attempts allowed: a foreign cancellation is retryable", calls)
	}
	if len(reader.committed) != 0 {
		t.Errorf("committed %v; a halted message must never be committed", reader.committed)
	}
}
