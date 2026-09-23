package saga

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedReader delivers a fixed list of messages and records what was
// committed. Once the list runs out it blocks on ctx, the way a real reader
// waits on an idle topic. Commits arrive from every partition's worker at
// once, so what it records is guarded.
type scriptedReader struct {
	messages  []Message
	next      int
	commitErr error
	// commitGate, when set, holds the first Commit call until it is closed —
	// a broker round trip that is slow to come back.
	commitGate chan struct{}

	mu          sync.Mutex
	committed   []Message
	commitCalls int
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

// Commit fails on a cancelled ctx, as kafka-go's does: a consumer that wants
// its last offsets recorded during shutdown has to ask with a live one.
func (r *scriptedReader) Commit(ctx context.Context, ms ...Message) error {
	r.mu.Lock()
	r.commitCalls++
	first := r.commitCalls == 1
	r.mu.Unlock()
	if first && r.commitGate != nil {
		select {
		case <-r.commitGate:
		case <-ctx.Done():
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.commitErr != nil {
		return r.commitErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.committed = append(r.committed, ms...)
	return nil
}

func (r *scriptedReader) commits() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.committed...)
}

// committedThrough reports the highest offset committed on each partition —
// what the group would resume after, since a commit covers everything before
// it on its partition.
func (r *scriptedReader) committedThrough() map[int]int64 {
	through := map[int]int64{}
	for _, m := range r.commits() {
		if prev, ok := through[m.Partition]; !ok || m.Offset > prev {
			through[m.Partition] = m.Offset
		}
	}
	return through
}

// waitCommittedThrough shuts ctx down once every partition in want has been
// committed at least that far, so a pass is not a wait for the timeout.
func waitCommittedThrough(ctx context.Context, cancel context.CancelFunc, r *scriptedReader, want map[int]int64) {
	for ctx.Err() == nil {
		through, done := r.committedThrough(), true
		for p, offset := range want {
			if got, ok := through[p]; !ok || got < offset {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
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
	if got := reader.committedThrough()[0]; got != 1 {
		t.Errorf("committed through offset %d, want 1: both messages", got)
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
	// Offset 8 may have been fetched — fetching moves nothing — but it must
	// never be handled: calls would count its attempts too.
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

func partitionMessage(partition int, offset int64) Message {
	return Message{
		Topic: "transfer-events", Partition: partition, Offset: offset,
		Key: []byte(fmt.Sprintf("p%d-%d", partition, offset)), Value: []byte(observedValue),
	}
}

// partitionOf reads back the partition partitionMessage wrote into the key.
func partitionOf(tr Trigger) string {
	p, _, _ := strings.Cut(tr.AggregateID, "-")
	return p
}

// Partitions are keyed by aggregate (root docs/adr/0001 decision 4), so two
// partitions never carry the same aggregate and nothing orders one against the
// other. A trigger held up on one must not hold up the rest: here partition 0's
// handler cannot finish until partition 1's has run, which a consumer handling
// one message at a time can never satisfy.
func TestConsumer_HandlesDifferentPartitionsConcurrently(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{partitionMessage(0, 0), partitionMessage(1, 0)}}
	partitionOneRan := make(chan struct{})
	c := quietConsumer(reader, handlerFunc(func(tr Trigger) error {
		if partitionOf(tr) == "p1" {
			close(partitionOneRan)
			return nil
		}
		select {
		case <-partitionOneRan:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("partition 1 never ran while partition 0 was in flight")
		}
	}), WithAttempts(1))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go waitCommittedThrough(ctx, cancel, reader, map[int]int64{0: 0, 1: 0})
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := len(reader.committedThrough()); got != 2 {
		t.Errorf("committed on %d partition(s), want both", got)
	}
}

// Within a partition, order is the one guarantee the publication gives
// (root docs/adr/0001 decisions 4 and 5), so it must survive concurrency: each
// partition's triggers are handled one at a time, in offset order, and
// committed in that order too, so a committed offset never passes one that has
// not been handled.
func TestConsumer_KeepsEachPartitionInOrderAndOneAtATime(t *testing.T) {
	t.Parallel()
	const partitions, perPartition = 3, 20
	var messages []Message
	for offset := int64(0); offset < perPartition; offset++ {
		for p := 0; p < partitions; p++ {
			messages = append(messages, partitionMessage(p, offset))
		}
	}
	reader := &scriptedReader{messages: messages}

	var (
		mu       sync.Mutex
		handled  = map[string][]string{}
		inFlight = map[string]int{}
		overlap  bool
	)
	c := quietConsumer(reader, handlerFunc(func(tr Trigger) error {
		p := partitionOf(tr)
		mu.Lock()
		inFlight[p]++
		if inFlight[p] > 1 {
			overlap = true
		}
		handled[p] = append(handled[p], tr.AggregateID)
		mu.Unlock()

		time.Sleep(100 * time.Microsecond) // long enough for an overlap to show

		mu.Lock()
		inFlight[p]--
		mu.Unlock()
		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	want := map[int]int64{}
	for p := 0; p < partitions; p++ {
		want[p] = perPartition - 1
	}
	go waitCommittedThrough(ctx, cancel, reader, want)
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if overlap {
		t.Error("two triggers from one partition were handled at the same time")
	}
	for p := 0; p < partitions; p++ {
		got := handled[fmt.Sprintf("p%d", p)]
		if len(got) != perPartition {
			t.Fatalf("partition %d: handled %d, want %d", p, len(got), perPartition)
		}
		for offset, id := range got {
			if want := fmt.Sprintf("p%d-%d", p, offset); id != want {
				t.Fatalf("partition %d: handled %s at position %d, want %s", p, id, offset, want)
			}
		}
	}
	last := map[int]int64{}
	for _, m := range reader.commits() {
		if prev, seen := last[m.Partition]; seen && m.Offset <= prev {
			t.Fatalf("partition %d committed offset %d after %d", m.Partition, m.Offset, prev)
		}
		last[m.Partition] = m.Offset
	}
}

// go/docs/adr/0003 halts the whole consumer, not one partition. But a trigger
// already in flight on another partition is not abandoned half way: it
// finishes and is committed, and Run does not return until it has, so a halt
// never leaves a handler running behind the process's back.
func TestConsumer_HaltFinishesWorkInFlightOnOtherPartitions(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{partitionMessage(1, 5), partitionMessage(0, 7)}}
	boom := errors.New("invariant broken")
	started, failed := make(chan struct{}), make(chan struct{})
	var inFlightReturned atomic.Bool
	c := quietConsumer(reader, handlerFunc(func(tr Trigger) error {
		if partitionOf(tr) == "p0" {
			// Fail only once partition 1 is genuinely in flight.
			select {
			case <-started:
			case <-time.After(2 * time.Second):
			}
			close(failed)
			return boom
		}
		close(started)
		select {
		case <-failed:
		case <-time.After(2 * time.Second):
			return errors.New("partition 0 never ran while partition 1 was in flight")
		}
		time.Sleep(20 * time.Millisecond) // still working as the halt lands
		inFlightReturned.Store(true)
		return nil
	}), WithAttempts(1))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Run(ctx)

	var halt *HaltError
	if !errors.As(err, &halt) || halt.Message.Partition != 0 || halt.Message.Offset != 7 {
		t.Fatalf("Run() error = %v, want a *HaltError on partition 0 offset 7", err)
	}
	if !inFlightReturned.Load() {
		t.Error("Run returned while partition 1's handler was still running")
	}
	commits := reader.commits()
	if len(commits) != 1 || commits[0].Partition != 1 {
		t.Errorf("committed %v, want only partition 1's finished message", commits)
	}
}

// A halt stops every partition taking new work. Messages already fetched for
// other partitions are left uncommitted for the next start, not handled.
func TestConsumer_HaltStopsOtherPartitionsTakingNewWork(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{
		partitionMessage(0, 0), partitionMessage(1, 0), partitionMessage(1, 1), partitionMessage(1, 2),
	}}
	failed := make(chan struct{})
	var afterHalt atomic.Int32
	c := quietConsumer(reader, handlerFunc(func(tr Trigger) error {
		switch tr.AggregateID {
		case "p0-0":
			return errors.New("invariant broken")
		case "p1-0":
			// Hold partition 1 until partition 0 has certainly halted.
			select {
			case <-failed:
			case <-time.After(2 * time.Second):
			}
			return nil
		default:
			afterHalt.Add(1)
			return nil
		}
	}), WithAttempts(1), WithLogger(log.New(haltSignal{failed: failed, once: &sync.Once{}}, "", 0)))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var halt *HaltError
	if err := c.Run(ctx); !errors.As(err, &halt) {
		t.Fatalf("Run() error = %v, want a *HaltError", err)
	}
	if n := afterHalt.Load(); n != 0 {
		t.Errorf("handled %d message(s) on partition 1 after the halt, want 0", n)
	}
}

// haltSignal closes failed when the consumer logs its halt — the moment every
// other partition must stop taking new work.
type haltSignal struct {
	failed chan struct{}
	once   *sync.Once
}

func (h haltSignal) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "HALTED") {
		h.once.Do(func() { close(h.failed) })
	}
	return len(p), nil
}

// Shutdown waits for handlers already running rather than returning over
// them: cmd/orchestrator closes the pool and the ledger client as soon as Run
// returns, which would pull both out from under a handler still using them.
func TestConsumer_ShutdownWaitsForHandlersInFlight(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{partitionMessage(0, 0)}}
	ctx, cancel := context.WithCancel(context.Background())
	var returned atomic.Bool
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		cancel()
		time.Sleep(20 * time.Millisecond)
		returned.Store(true)
		return nil
	}))

	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil on shutdown", err)
	}
	if !returned.Load() {
		t.Error("Run returned while a handler was still running")
	}
}

// A commit is a broker round trip, and a partition's next trigger does not
// depend on the last one's offset being recorded — only on its side effects
// having happened. So handling carries on while a commit is still out: here
// the first commit does not come back until all ten triggers are handled,
// which a worker that waited on each commit could never do.
func TestConsumer_HandlingDoesNotWaitOnACommitInFlight(t *testing.T) {
	t.Parallel()
	var messages []Message
	for offset := int64(0); offset < 10; offset++ {
		messages = append(messages, partitionMessage(0, offset))
	}
	gate := make(chan struct{})
	reader := &scriptedReader{messages: messages, commitGate: gate}
	var handled atomic.Int32
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		if handled.Add(1) == 10 {
			close(gate)
		}
		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go waitCommittedThrough(ctx, cancel, reader, map[int]int64{0: 9})
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if n := handled.Load(); n != 10 {
		t.Fatalf("handled %d triggers while the first commit was held, want all 10", n)
	}
	if got := reader.committedThrough()[0]; got != 9 {
		t.Errorf("committed through offset %d, want 9", got)
	}
}

// Offsets that pile up behind a commit in flight go out together in the next
// one, as a single high-water mark per partition, rather than one round trip
// each.
func TestConsumer_CoalescesOffsetsQueuedBehindACommit(t *testing.T) {
	t.Parallel()
	const partitions, perPartition = 3, 10
	var messages []Message
	for offset := int64(0); offset < perPartition; offset++ {
		for p := 0; p < partitions; p++ {
			messages = append(messages, partitionMessage(p, offset))
		}
	}
	gate := make(chan struct{})
	reader := &scriptedReader{messages: messages, commitGate: gate}
	var handled atomic.Int32
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		if handled.Add(1) == partitions*perPartition {
			close(gate)
		}
		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	want := map[int]int64{}
	for p := 0; p < partitions; p++ {
		want[p] = perPartition - 1
	}
	go waitCommittedThrough(ctx, cancel, reader, want)
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	reader.mu.Lock()
	calls := reader.commitCalls
	reader.mu.Unlock()
	// The held first commit, one carrying everything queued behind it, and at
	// most one more: the gate opens as the last handler runs, so its offset
	// can arrive just after that drain. One call per trigger would be 30.
	if calls > 3 {
		t.Errorf("%d commit calls for %d triggers, want them coalesced into at most 3", calls, partitions*perPartition)
	}
	for p, offset := range reader.committedThrough() {
		if offset != perPartition-1 {
			t.Errorf("partition %d committed through %d, want %d", p, offset, perPartition-1)
		}
	}
}

// Shutdown records what was handled before it returns, rather than leaving it
// all to be redelivered: the handler ran, so its offset is owed.
func TestConsumer_ShutdownCommitsWhatWasHandled(t *testing.T) {
	t.Parallel()
	reader := &scriptedReader{messages: []Message{partitionMessage(0, 4)}}
	ctx, cancel := context.WithCancel(context.Background())
	c := quietConsumer(reader, handlerFunc(func(Trigger) error {
		cancel()
		return nil
	}))

	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil on shutdown", err)
	}
	if got, ok := reader.committedThrough()[0]; !ok || got != 4 {
		t.Errorf("committed %v, want offset 4: it was handled before shutdown", reader.commits())
	}
}
