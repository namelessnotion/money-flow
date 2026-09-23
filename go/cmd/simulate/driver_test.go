package main

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// The tests in this package run the tool in-process, with no Kafka and no
// cmd/orchestrator. publishingStore and startOrchestrator stand in for both:
// every Transfer or Transaction append publishes a trigger, as the CDC
// connector does, and a goroutine delivers those triggers to the real
// saga.Orchestrator, as cmd/orchestrator does.
//
// Delivery is asynchronous on purpose. Every RPC the tool makes returns before
// anything it caused has been folded — the production shape since
// go/docs/adr/0006, and the one the tool's waiting exists to cope with. A
// stand-in that drove the saga inside the RPC would answer every first look
// with a state production never shows that early, and hide exactly the bugs
// that waiting is for.
type publishingStore struct {
	eventstore.Store

	mu      sync.Mutex
	pending []saga.Trigger
	wake    chan struct{}
}

func newPublishingStore(inner eventstore.Store) *publishingStore {
	return &publishingStore{Store: inner, wake: make(chan struct{}, 1)}
}

func (s *publishingStore) Append(
	ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message,
) error {
	if err := s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...); err != nil {
		return err
	}
	if len(events) > 0 {
		s.publish(aggregateType, aggregateID)
	}
	return nil
}

func (s *publishingStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	if err := s.Store.AppendAtomic(ctx, writes...); err != nil {
		return err
	}
	for _, w := range writes {
		if len(w.Events) > 0 {
			s.publish(w.AggregateType, w.AggregateID)
		}
	}
	return nil
}

// publish queues a trigger for the two aggregate types the orchestrator
// consumes. Holders, Wallets and Tokens are published to topics nothing here
// reads, and handing one to the orchestrator would be an error it is right to
// raise.
func (s *publishingStore) publish(aggregateType, aggregateID string) {
	if aggregateType != transfer.AggregateType && aggregateType != transaction.AggregateType {
		return
	}
	s.mu.Lock()
	s.pending = append(s.pending, saga.Trigger{AggregateType: aggregateType, AggregateID: aggregateID})
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default: // a wake-up is already pending, and it will see this trigger too
	}
}

// take hands back everything published since the last call, once per
// aggregate. A trigger means only "this aggregate moved" (go/docs/adr/0001),
// so delivering several for the same aggregate back to back adds nothing.
func (s *publishingStore) take() []saga.Trigger {
	s.mu.Lock()
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()

	seen := make(map[saga.Trigger]bool, len(batch))
	unique := batch[:0]
	for _, t := range batch {
		if !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}
	return unique
}

// startOrchestrator delivers store's triggers to o until the test ends. A
// trigger that keeps failing fails the test: in production it would halt the
// consumer (go/docs/adr/0003), which is never something to step over quietly.
func startOrchestrator(t *testing.T, store *publishingStore, o *saga.Orchestrator) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-store.wake:
			}
			for _, trigger := range store.take() {
				if err := handleWithRetries(ctx, o, trigger); err != nil && ctx.Err() == nil {
					t.Errorf("orchestrator: %s: %v", trigger, err)
				}
			}
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// handleWithRetries retries a failed trigger a few times before giving up, the
// way cmd/orchestrator does before it halts (go/docs/adr/0003). It matters here
// for the same reason it matters there: a Transfer whose prepare step loses a
// concurrency race on a hot Wallet fails its handler and succeeds on the next
// attempt, and treating that as fatal would make these tests fail on
// scheduling luck rather than on anything about the code.
func handleWithRetries(ctx context.Context, o *saga.Orchestrator, t saga.Trigger) error {
	const attempts = 4
	var err error
	for i := 0; i < attempts; i++ {
		if err = o.Handle(ctx, t); err == nil {
			return nil
		}
	}
	return err
}
