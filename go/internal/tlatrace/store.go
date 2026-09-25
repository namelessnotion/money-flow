package tlatrace

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

// Store is an eventstore.Store that records each Load and Append after the
// database answers it, attributed to the Request its context carries.
//
// It must sit directly on the real store, beneath anything that injects
// faults, so that a trace records what the database did rather than what a
// handler was told: an append that landed but whose reply was lost is
// recorded as landed.
type Store struct {
	eventstore.Store
	rec *Recorder
}

func NewStore(inner eventstore.Store, rec *Recorder) *Store {
	return &Store{Store: inner, rec: rec}
}

// Load records the stream length and event types the caller will decide
// against. A failed Load observed nothing and is not recorded.
func (s *Store) Load(ctx context.Context, aggregateType, aggregateID string) ([]eventstore.Event, error) {
	events, err := s.Store.Load(ctx, aggregateType, aggregateID)
	if err != nil {
		return events, err
	}
	req, _ := RequestFrom(ctx)
	n := len(events)
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.EventType
	}
	s.rec.record(Record{Kind: KindLoad, Req: req, Stream: aggregateID, Len: &n, Types: types})
	return events, nil
}

// Append records the expected sequence, the event types and whether the
// INSERT landed, lost its sequence to another writer, or failed.
func (s *Store) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	err := s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	req, _ := RequestFrom(ctx)
	s.rec.record(Record{
		Kind: KindAppend, Req: req, Stream: aggregateID,
		ExpectedSeq: &expectedSeq, Types: eventTypes(events), Outcome: outcome(err),
	})
	return err
}

// AppendAtomic is outside the design spec/eventstore.tla models. It is
// recorded with every stream it wrote, so a trace that holds one is rejected
// rather than quietly missing a write.
func (s *Store) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	err := s.Store.AppendAtomic(ctx, writes...)
	req, _ := RequestFrom(ctx)
	streams := make([]string, len(writes))
	for i, w := range writes {
		streams[i] = w.AggregateID
	}
	s.rec.record(Record{Kind: KindAppendAtomic, Req: req, Streams: streams, Outcome: outcome(err)})
	return err
}

func outcome(err error) string {
	switch {
	case err == nil:
		return OutcomeOK
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		return OutcomeConflict
	default:
		return OutcomeError
	}
}

func eventTypes(events []proto.Message) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = eventstore.EventType(e)
	}
	return types
}
