package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// settle is how long a test waits for something that should happen almost
// immediately, and how long it is prepared to wait before calling a hang a
// hang.
const settle = 2 * time.Second

// fakeReader is one topic's delivery, scripted: it hands over whatever a test
// puts on its channel and otherwise blocks, the way a real reader waits on an
// idle topic.
type fakeReader struct {
	messages chan saga.Message

	mu     sync.Mutex
	closed bool
}

func (r *fakeReader) Fetch(ctx context.Context) (saga.Message, error) {
	select {
	case m := <-r.messages:
		return m, nil
	case <-ctx.Done():
		return saga.Message{}, ctx.Err()
	}
}

func (r *fakeReader) Commit(context.Context, saga.Message) error { return nil }

func (r *fakeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *fakeReader) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// fakeTransport stands in for Kafka. Which topics exist yet is the test's to
// decide, and every reader it opens is kept so a test can look at it.
type fakeTransport struct {
	// waits, keyed by topic, hold a topic that has not been published yet;
	// closing one publishes it. A topic with no entry exists from the start.
	waits    map[string]chan struct{}
	waitErrs map[string]error

	mu      sync.Mutex
	readers map[string]*fakeReader
	waiting map[string]bool
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		waits:    map[string]chan struct{}{},
		waitErrs: map[string]error{},
		readers:  map[string]*fakeReader{},
		waiting:  map[string]bool{},
	}
}

// missing marks topic as not yet published. Call before starting run.
func (tr *fakeTransport) missing(topic string) { tr.waits[topic] = make(chan struct{}) }

// fails marks topic's wait as broken rather than merely slow.
func (tr *fakeTransport) fails(topic string, err error) { tr.waitErrs[topic] = err }

// publish releases whatever is waiting on topic.
func (tr *fakeTransport) publish(topic string) { close(tr.waits[topic]) }

func (tr *fakeTransport) transport() transport {
	return transport{
		waitForTopic: func(ctx context.Context, topic string) error {
			if err := tr.waitErrs[topic]; err != nil {
				return err
			}
			wait, blocked := tr.waits[topic]
			if !blocked {
				return nil
			}
			tr.markWaiting(topic)
			select {
			case <-wait:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		newReader: func(_, topic string) topicReader {
			r := &fakeReader{messages: make(chan saga.Message, 1)}
			tr.mu.Lock()
			defer tr.mu.Unlock()
			tr.readers[topic] = r
			return r
		},
	}
}

func (tr *fakeTransport) markWaiting(topic string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.waiting[topic] = true
}

func (tr *fakeTransport) isWaiting(topic string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.waiting[topic]
}

func (tr *fakeTransport) reader(topic string) *fakeReader {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.readers[topic]
}

// handlerFunc adapts a plain function to saga.Handler.
type handlerFunc func(saga.Trigger) error

func (f handlerFunc) Handle(_ context.Context, t saga.Trigger) error { return f(t) }

func trigger(aggregateType, id string) saga.Message {
	return saga.Message{
		Topic: saga.Topic(aggregateType),
		Key:   []byte(id),
		Value: []byte(fmt.Sprintf(`{"aggregate_type":%q,"event_type":"Moved","sequence":1,"global_seq":1}`, aggregateType)),
	}
}

// start runs run in the background, returning the func that stops it and the
// func that reports what it returned.
func start(t *testing.T, tr transport, handler saga.Handler) (stop context.CancelFunc, returned func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx, tr, handler) }()

	return cancel, func() error {
		t.Helper()
		select {
		case err := <-done:
			return err
		case <-time.After(settle):
			t.Fatal("run() did not return")
			return nil
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The regression this file exists for. Waited for in sequence, one missing
// topic held up every consumer behind it — and on a fresh CDC database that is
// the ordinary starting state: nothing has been published to transfer-events
// until a Transaction dispatches its first child, and after the cutover only a
// consumed transaction trigger can make it do so. So the transaction topic has
// to be consumed while the transfer topic is still missing, or neither ever is.
func TestRunConsumesOneTopicWhileAnotherIsStillMissing(t *testing.T) {
	t.Parallel()

	transfers, transactions := saga.Topic(transfer.AggregateType), saga.Topic(transaction.AggregateType)
	fake := newFakeTransport()
	fake.missing(transfers)

	handled := make(chan saga.Trigger, 1)
	stop, returned := start(t, fake.transport(), handlerFunc(func(tr saga.Trigger) error {
		handled <- tr
		return nil
	}))

	waitFor(t, "the transaction reader to open", func() bool { return fake.reader(transactions) != nil })
	fake.reader(transactions).messages <- trigger(transaction.AggregateType, "txn1")

	select {
	case got := <-handled:
		if got.AggregateID != "txn1" {
			t.Errorf("handled %s, want txn1", got)
		}
	case <-time.After(settle):
		t.Fatal("nothing was consumed from the transaction topic while the transfer topic was missing")
	}

	// The missing topic is still being waited for, not skipped: it is consumed
	// as soon as it appears.
	if !fake.isWaiting(transfers) {
		t.Fatalf("%s was not waited for", transfers)
	}
	fake.publish(transfers)
	waitFor(t, "the transfer reader to open", func() bool { return fake.reader(transfers) != nil })

	stop()
	if err := returned(); err != nil {
		t.Errorf("run() error = %v, want nil on shutdown", err)
	}
}

// Shutdown before either topic exists is an ordinary stop, not a failure.
func TestRunStopsCleanlyWhileWaitingForTopics(t *testing.T) {
	t.Parallel()

	fake := newFakeTransport()
	for _, aggregateType := range consumedAggregateTypes {
		fake.missing(saga.Topic(aggregateType))
	}

	stop, returned := start(t, fake.transport(), handlerFunc(func(saga.Trigger) error { return nil }))
	waitFor(t, "both waits to start", func() bool {
		for _, aggregateType := range consumedAggregateTypes {
			if !fake.isWaiting(saga.Topic(aggregateType)) {
				return false
			}
		}
		return true
	})
	stop()

	if err := returned(); err != nil {
		t.Errorf("run() error = %v, want nil on shutdown", err)
	}
}

// A wait that fails outright is a real failure and has to surface, even while
// the other topic is still waiting and would otherwise never return.
func TestRunReportsAFailedWait(t *testing.T) {
	t.Parallel()

	unreachable := errors.New("brokers unreachable")
	fake := newFakeTransport()
	fake.missing(saga.Topic(transfer.AggregateType))
	fake.fails(saga.Topic(transaction.AggregateType), unreachable)

	_, returned := start(t, fake.transport(), handlerFunc(func(saga.Trigger) error { return nil }))

	if err := returned(); !errors.Is(err, unreachable) {
		t.Errorf("run() error = %v, want %v", err, unreachable)
	}
}

// One consumer giving up stops the other, and every reader that was opened is
// released: a Transaction that has stopped hearing about its children is not
// usefully still running.
func TestRunStopsEveryConsumerWhenOneHalts(t *testing.T) {
	t.Parallel()

	transfers := saga.Topic(transfer.AggregateType)
	fake := newFakeTransport()
	_, returned := start(t, fake.transport(), handlerFunc(func(saga.Trigger) error { return nil }))

	waitFor(t, "the transfer reader to open", func() bool { return fake.reader(transfers) != nil })
	// A message with no key names no aggregate, which halts rather than skips.
	fake.reader(transfers).messages <- saga.Message{Topic: transfers}

	var halted *saga.HaltError
	if err := returned(); !errors.As(err, &halted) {
		t.Fatalf("run() error = %v, want a *saga.HaltError", err)
	}
	for _, aggregateType := range consumedAggregateTypes {
		topic := saga.Topic(aggregateType)
		reader := fake.reader(topic)
		if reader == nil {
			t.Errorf("%s was never consumed", topic)
			continue
		}
		if !reader.isClosed() {
			t.Errorf("%s reader was left open", topic)
		}
	}
}
