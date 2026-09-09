package saga

import (
	"context"
	"fmt"
	"log"
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
type Reader interface {
	// Fetch blocks until the next message is available, ctx is cancelled, or
	// the reader fails. It does not advance any committed position.
	Fetch(ctx context.Context) (Message, error)

	// Commit records m, and everything before it on m's partition, as
	// processed.
	Commit(ctx context.Context, m Message) error
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
func (c *Consumer) Run(ctx context.Context) error {
	for {
		msg, err := c.reader.Fetch(ctx)
		if err != nil {
			if isShutdown(ctx) {
				return nil
			}
			return fmt.Errorf("saga: fetch: %w", err)
		}

		if err := c.process(ctx, msg); err != nil {
			if isShutdown(ctx) {
				return nil
			}
			c.logger.Printf("saga: HALTED on %s; nothing further will be consumed from this reader: %v", msg, err)
			return err
		}

		// After the side effects, never before: a crash between the two
		// redelivers the message, and redelivery re-folds to the same place.
		if err := c.reader.Commit(ctx, msg); err != nil {
			if isShutdown(ctx) {
				return nil
			}
			return fmt.Errorf("saga: commit %s: %w", msg, err)
		}
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
