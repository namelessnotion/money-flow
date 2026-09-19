package eventstore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// MemoryStore is an in-process Store intended for tests: it lets a service be
// exercised without a database while still enforcing the same sequencing and
// optimistic-concurrency rules as PostgresStore. Nothing here is durable.
type MemoryStore struct {
	mu        sync.Mutex
	streams   map[string][]Event
	globalSeq int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{streams: make(map[string][]Event)}
}

func (s *MemoryStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	return s.AppendAtomic(ctx, StreamWrite{
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		ExpectedSeq:   expectedSeq,
		Events:        events,
	})
}

func (s *MemoryStore) AppendAtomic(ctx context.Context, writes ...StreamWrite) error {
	writes = liveWrites(writes)
	if err := validateWrites(writes); err != nil {
		return err
	}
	if len(writes) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate and marshal every stream before mutating any of them, so a
	// failure part-way through can't leave a partially-applied batch behind.
	// This is the in-memory stand-in for PostgresStore's transaction.
	//
	// Indexed by write position rather than keyed by stream: global_seq is
	// assigned in this order below, and ranging a map would make it
	// nondeterministic and diverge from Postgres' insert order.
	staged := make([][]Event, len(writes))
	for n, w := range writes {
		// Still stricter than PostgresStore: an ExpectedSeq that skips past the
		// end of the stream is rejected here, where Postgres would accept it
		// and leave a gap.
		if int64(len(s.streams[streamKey(w.AggregateType, w.AggregateID)])) != w.ExpectedSeq {
			return ErrConcurrencyConflict
		}

		batch := make([]Event, 0, len(w.Events))
		for i, evt := range w.Events {
			payload, err := proto.Marshal(evt)
			if err != nil {
				return fmt.Errorf("eventstore: marshal %T: %w", evt, err)
			}
			batch = append(batch, Event{
				AggregateType: w.AggregateType,
				AggregateID:   w.AggregateID,
				Sequence:      w.ExpectedSeq + int64(i) + 1,
				EventType:     EventType(evt),
				Payload:       payload,
				OccurredAt:    time.Now().UTC(),
			})
		}
		staged[n] = batch
	}

	for n, w := range writes {
		key := streamKey(w.AggregateType, w.AggregateID)
		for i := range staged[n] {
			s.globalSeq++
			staged[n][i].GlobalSeq = s.globalSeq
		}
		s.streams[key] = append(s.streams[key], staged[n]...)
	}
	return nil
}

func (s *MemoryStore) Load(ctx context.Context, aggregateType, aggregateID string) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stream := s.streams[streamKey(aggregateType, aggregateID)]
	if len(stream) == 0 {
		return nil, nil
	}
	return append([]Event(nil), stream...), nil
}

// Now returns the Go process's own clock: MemoryStore stamps Event.OccurredAt
// with time.Now().UTC() in AppendAtomic, so comparing against the same clock
// here is self-consistent by construction — unlike PostgresStore, there is no
// second clock to drift against.
func (s *MemoryStore) Now(ctx context.Context) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Now().UTC(), nil
}

// streamKey uses a NUL separator so it can't collide with an aggregateType
// containing the separator character.
func streamKey(aggregateType, aggregateID string) string {
	return aggregateType + "\x00" + aggregateID
}
