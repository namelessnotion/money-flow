package saga

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Message is one delivered record, in the only terms this package needs:
// where it came from, so a halt can name it, and the key and body ParseTrigger
// reads.
type Message struct {
	Topic     string
	Partition int
	Offset    int64
	Key       []byte
	Value     []byte
}

func (m Message) String() string {
	return fmt.Sprintf("%s[%d]@%d", m.Topic, m.Partition, m.Offset)
}

// Reader is the delivery side of the transport, narrow enough that the
// orchestrator's own behaviour can be exercised without a broker.
//
// Fetch and Commit are deliberately separate calls: committing only after the
// handler has returned is what makes delivery at-least-once, which
// go/docs/adr/0001 decision 3 permits and go/docs/adr/0003 relies on.
//
// Fetch is only ever called from one goroutine, but Commit is called from one
// per partition at once, so it must be safe for concurrent use.
type Reader interface {
	// Fetch blocks until the next message is available, ctx is cancelled, or
	// the reader fails. It does not advance any committed position.
	Fetch(ctx context.Context) (Message, error)

	// Commit records each of ms, and everything before it on its partition,
	// as processed, in one call.
	Commit(ctx context.Context, ms ...Message) error
}

// Handler drives whatever a trigger names. *Orchestrator is the implementation.
type Handler interface {
	Handle(ctx context.Context, t Trigger) error
}

const (
	// defaultAttempts is how many times a handler failure is retried before the
	// consumer halts. Small on purpose: a trigger re-folds authoritative state,
	// so a failure that survives a few immediate retries is a real fault — a
	// database that is down, or an invariant that has broken — not contention
	// waiting to clear.
	defaultAttempts = 3

	// defaultBackoff is the pause before the first retry; each subsequent
	// retry doubles it.
	defaultBackoff = 250 * time.Millisecond

	// commitBacklog is how many handled messages may wait for the committer
	// before a worker blocks handing it another. The committer drains the
	// whole backlog into each commit, so this bounds one commit's size and the
	// redelivery a crash can cause, not the commit rate.
	commitBacklog = 1024

	// flushTimeout bounds the last commit on the way out, which runs after
	// shutdown has cancelled ctx: long enough for one round trip to the group
	// coordinator, short enough not to hold up a SIGTERM.
	flushTimeout = 5 * time.Second

	// partitionBacklog is how many fetched messages may wait on one partition
	// while its worker is busy. Fetching is a single stream across every
	// partition, so a partition whose backlog is full stalls the fetch for all
	// of them; a small queue absorbs an ordinary run of messages on one key
	// without that. It bounds memory, not correctness: a waiting message is
	// uncommitted, and a halt or shutdown leaves it for the next start.
	partitionBacklog = 64
)

// HaltError reports that a consumer stopped on a message it could neither
// process nor pass over, and names the exact message so an operator can go and
// look at it.
type HaltError struct {
	Message  Message
	Attempts int
	Err      error
}

func (e *HaltError) Error() string {
	return fmt.Sprintf("saga: halted on %s after %d attempt(s): %v", e.Message, e.Attempts, e.Err)
}

func (e *HaltError) Unwrap() error { return e.Err }

// Consumer reads messages and hands each to a Handler, halting rather than
// skipping when one cannot be processed.
//
// Halting is the decision recorded in go/docs/adr/0003, and it is the whole
// reason this type exists rather than a bare loop. The alternatives are worse
// for a system that moves money: skipping a trigger loses the only wake-up an
// aggregate was going to get, and dead-lettering is the same loss with extra
// machinery in front of it. Stopping is loud, costs nothing but availability,
// and leaves the uncommitted offset exactly where the next start resumes from.
type Consumer struct {
	reader   Reader
	handler  Handler
	attempts int
	backoff  time.Duration
	logger   *log.Logger
}

// ConsumerOption adjusts a Consumer at construction.
type ConsumerOption func(*Consumer)

// WithAttempts sets how many times a handler failure is retried before the
// consumer halts. attempts must be at least 1.
func WithAttempts(attempts int) ConsumerOption {
	return func(c *Consumer) {
		if attempts >= 1 {
			c.attempts = attempts
		}
	}
}

// WithBackoff sets the pause before the first retry; each subsequent retry
// doubles it.
func WithBackoff(d time.Duration) ConsumerOption {
	return func(c *Consumer) { c.backoff = d }
}

// WithLogger redirects the consumer's own reporting.
func WithLogger(l *log.Logger) ConsumerOption {
	return func(c *Consumer) { c.logger = l }
}

func NewConsumer(reader Reader, handler Handler, opts ...ConsumerOption) *Consumer {
	c := &Consumer{
		reader:   reader,
		handler:  handler,
		attempts: defaultAttempts,
		backoff:  defaultBackoff,
		logger:   log.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run consumes until ctx is cancelled, the reader fails, or a message halts
// it. A cancelled context is an ordinary shutdown and returns nil; anything
// else is returned, with a *HaltError distinguishing "this message stopped us"
// from "the transport did".
//
// Each partition is consumed by its own worker, one message at a time and in
// offset order, and the workers run concurrently. That is exactly as much order
// as the publication promises: messages are keyed by aggregate id, so one
// aggregate's triggers share a partition and stay in sequence (root
// docs/adr/0001 decisions 4 and 5), and nothing orders one partition against
// another. Handling across aggregates was already concurrent — the transfer and
// transaction topics are consumed side by side, and go/docs/adr/0005 made
// concurrent dispatch converge — so this adds no interleaving a single-partition
// topic could not already produce.
//
// A halt on any partition stops every partition taking new work, as
// go/docs/adr/0003 decides. Work already in flight elsewhere is not abandoned:
// it finishes and commits. On any exit, Run returns only after every worker has
// stopped, because cmd/orchestrator releases the pool and ledger client the
// handlers use as soon as it does.
func (c *Consumer) Run(ctx context.Context) error {
	// Fetching stops on shutdown or on the first failure, whichever comes
	// first; a failure cancels only the fetch, never ctx, so handlers in
	// flight on other partitions run to completion.
	fetchCtx, stopFetching := context.WithCancel(ctx)
	defer stopFetching()

	var (
		wg       sync.WaitGroup
		failOnce sync.Once
		failure  error
		failed   = make(chan struct{})
		backlogs = map[int]chan Message{}
	)
	fail := func(err error) {
		failOnce.Do(func() {
			failure = err
			close(failed)
			stopFetching()
		})
	}

	handled := make(chan Message, commitBacklog)
	committerDone := make(chan struct{})
	go func() {
		defer close(committerDone)
		c.commitHandled(ctx, handled, fail)
	}()

	var fetchErr error
	for {
		var msg Message
		if msg, fetchErr = c.reader.Fetch(fetchCtx); fetchErr != nil {
			break
		}

		backlog, ok := backlogs[msg.Partition]
		if !ok {
			backlog = make(chan Message, partitionBacklog)
			backlogs[msg.Partition] = backlog
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.consumePartition(ctx, backlog, handled, failed, fail)
			}()
		}
		select {
		case backlog <- msg:
		case <-failed:
		case <-ctx.Done():
		}
	}

	// Closing every backlog is what lets an idle worker return. Once all of
	// them have, nothing more can be handled, so the committer is told to
	// flush what it holds and stop; only after that is failure settled, since
	// until then a worker or the committer could still set it.
	for _, backlog := range backlogs {
		close(backlog)
	}
	wg.Wait()
	close(handled)
	<-committerDone

	switch {
	case failure != nil:
		return failure
	case isShutdown(ctx):
		return nil
	default:
		return fmt.Errorf("saga: fetch: %w", fetchErr)
	}
}

// consumePartition handles one partition's messages in the order they were
// fetched, handing each to the committer only after its side effects: a crash
// before the commit redelivers the message, and redelivery re-folds to the
// same place. It does not wait for the commit itself — the next trigger needs
// the last one's side effects, not its offset — so a commit's round trip is
// off every partition's critical path. It stops at the first message it
// cannot process, and before taking any further message once another
// partition, or the committer, has failed.
func (c *Consumer) consumePartition(
	ctx context.Context, backlog <-chan Message, handled chan<- Message, failed <-chan struct{}, fail func(error),
) {
	for msg := range backlog {
		select {
		case <-failed:
			return
		default:
		}
		if isShutdown(ctx) {
			return
		}

		if err := c.process(ctx, msg); err != nil {
			if isShutdown(ctx) {
				return
			}
			// Stop the other partitions first, then say so: once the halt is
			// visible, nothing else should still be starting.
			fail(err)
			c.logger.Printf("saga: HALTED on %s; nothing further will be consumed from this reader: %v", msg, err)
			return
		}

		// Handed over first, even mid-halt: this message's side effects have
		// happened, so its offset is owed. Only a full backlog — which means
		// the committer itself has stopped — is a reason to give up on it.
		select {
		case handled <- msg:
		default:
			select {
			case handled <- msg:
			case <-failed:
				return
			}
		}
	}
}

// commitHandled records handled messages' offsets until handled is closed,
// then flushes what is left.
//
// Each commit carries everything that queued up while the previous one was out
// — one high-water mark per partition, since a commit covers everything before
// it on its partition — so under load commits grow rather than multiply, and
// a single round trip is shared by every partition. Offsets on a partition
// arrive in the order its worker handled them, so the high-water mark never
// passes a message that has not been handled.
//
// A commit that fails stops the consumer (go/docs/adr/0003 decision 5): a
// consumer that cannot record progress would replay from its last commit on
// every restart. The final flush runs after shutdown has cancelled ctx, so it
// gets a context of its own; if even that fails, the offsets it held are
// simply redelivered on the next start.
func (c *Consumer) commitHandled(ctx context.Context, handled <-chan Message, fail func(error)) {
	pending := map[int]Message{}
	take := func(m Message) {
		if prev, ok := pending[m.Partition]; !ok || m.Offset > prev.Offset {
			pending[m.Partition] = m
		}
	}
	commit := func(ctx context.Context) error {
		if len(pending) == 0 {
			return nil
		}
		batch := make([]Message, 0, len(pending))
		for _, m := range pending {
			batch = append(batch, m)
		}
		if err := c.reader.Commit(ctx, batch...); err != nil {
			return fmt.Errorf("saga: commit %v: %w", batch, err)
		}
		clear(pending)
		return nil
	}

	for m := range handled {
		take(m)
		for drained := false; !drained; {
			select {
			case more, ok := <-handled:
				if !ok {
					drained = true
					break
				}
				take(more)
			default:
				drained = true
			}
		}
		if err := commit(ctx); err != nil {
			if isShutdown(ctx) {
				break // left for the final flush below
			}
			// Workers see the failure rather than block handing over more.
			fail(err)
			return
		}
	}

	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()
	for m := range handled {
		take(m)
	}
	if err := commit(flushCtx); err != nil {
		c.logger.Printf("saga: final commit failed; these offsets will be redelivered: %v", err)
	}
}

// process parses and handles one message, retrying a handler failure a bounded
// number of times before giving up.
//
// A message that will not parse is not retried at all. Retrying exists to ride
// out a transient fault in something the handler depends on; a malformed
// envelope is not transient, and no number of attempts will change it.
func (c *Consumer) process(ctx context.Context, msg Message) error {
	trigger, err := ParseTrigger(msg.Key, msg.Value)
	if err != nil {
		return &HaltError{Message: msg, Attempts: 0, Err: err}
	}

	backoff := c.backoff
	var lastErr error
	for attempt := 1; attempt <= c.attempts; attempt++ {
		if attempt > 1 {
			c.logger.Printf("saga: retrying %s (%s), attempt %d of %d, after: %v", msg, trigger, attempt, c.attempts, lastErr)
			if err := sleep(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
		}

		lastErr = c.handler.Handle(ctx, trigger)
		if lastErr == nil {
			// One line per trigger. An event log is low-volume, and root
			// docs/adr/0001 decision 6 chose process-manager orchestration
			// partly for central logging — a consumer whose only output is
			// silence gives an operator no way to tell "nothing to do" from
			// "receiving nothing".
			c.logger.Printf("saga: handled %s: %s", msg, trigger)
			return nil
		}
		if isShutdown(ctx) {
			return lastErr
		}
	}
	return &HaltError{Message: msg, Attempts: c.attempts, Err: lastErr}
}

// isShutdown reports whether this consumer's own ctx is why the caller's
// operation returned, rather than anything having gone wrong.
//
// Only ctx.Err() is checked, deliberately. ctx.Err() becomes non-nil before
// ctx.Done() closes, so any operation that observes this ctx's cancellation
// and returns will see it set — checking the returned error too would let a
// context.Canceled wrapped from an unrelated child context (a handler's own
// timeout, a fetch or commit's internal cancellation) masquerade as this
// consumer shutting down. That misclassifies a live dependency failure as a
// clean stop: Run returns nil, cmd/orchestrator records nothing and never
// cancels the sibling consumer, and the process stays up with one topic no
// longer consumed. That is the silent loss go/docs/adr/0003 halts to avoid.
//
// The same reasoning covers context.DeadlineExceeded: a deadline arriving
// from below — a per-message timeout, a connect_timeout, any deadline inside
// the handler — is a dependency failing and must halt, not be swallowed as
// shutdown.
func isShutdown(ctx context.Context) bool {
	return ctx.Err() != nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
