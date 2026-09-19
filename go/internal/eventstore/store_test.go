package eventstore_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"google.golang.org/protobuf/proto"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

func TestEventType(t *testing.T) {
	t.Parallel()

	got := eventstore.EventType(&pb.HolderEstablished{Id: "h1"})
	want := "holder.v1.HolderEstablished"
	if got != want {
		t.Fatalf("EventType() = %q, want %q", got, want)
	}
}

func TestEventDecode(t *testing.T) {
	t.Parallel()

	original := &pb.HolderEstablished{
		Id:    "h1",
		Memos: map[string]*pb.Memo{"why": {Id: "m1", Value: "onboarding"}},
	}
	payload, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	event := eventstore.Event{EventType: eventstore.EventType(original), Payload: payload}

	msg, err := event.Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	decoded, ok := msg.(*pb.HolderEstablished)
	if !ok {
		t.Fatalf("Decode() returned %T, want *pb.HolderEstablished", msg)
	}
	if !proto.Equal(original, decoded) {
		t.Errorf("round-trip mismatch:\n got: %v\nwant: %v", decoded, original)
	}
}

func TestEventDecodeUnknownType(t *testing.T) {
	t.Parallel()

	event := eventstore.Event{EventType: "holder.v1.NoSuchEvent", Payload: []byte{}}

	if _, err := event.Decode(); err == nil {
		t.Fatal("Decode() error = nil, want an error for an unregistered event type")
	}
}

// A payload that can't be unmarshalled must surface as an error rather than a
// zero-valued event: silently returning empty domain events would be worse than
// failing loudly.
func TestEventDecodeCorruptPayload(t *testing.T) {
	t.Parallel()

	event := eventstore.Event{
		EventType: eventstore.EventType(&pb.HolderEstablished{}),
		Payload:   []byte{0xff, 0xff, 0xff, 0xff},
	}

	if _, err := event.Decode(); err == nil {
		t.Fatal("Decode() error = nil, want an error for a corrupt payload")
	}
}

func TestClaimWinsAtTheExpectedSequence(t *testing.T) {
	t.Parallel()

	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	id := uniqueID(t)

	won, err := eventstore.Claim(ctx, store, "holder", id, 0, &pb.HolderEstablished{Id: id})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if !won {
		t.Fatal("Claim() won = false, want true for the first claim on a fresh stream")
	}

	events, err := store.Load(ctx, "holder", id)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Load() returned %d events, want 1", len(events))
	}
}

// A second caller racing the same (aggregateType, aggregateID, expectedSeq)
// loses without error: this is the convergence contract go/docs/adr/0005
// relies on — the loser treats losing as "someone else is handling this",
// not as failure.
func TestClaimLoserConvergesWithoutError(t *testing.T) {
	t.Parallel()

	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	id := uniqueID(t)

	first, err := eventstore.Claim(ctx, store, "holder", id, 0, &pb.HolderEstablished{Id: id})
	if err != nil || !first {
		t.Fatalf("first Claim() = (%v, %v), want (true, nil)", first, err)
	}

	second, err := eventstore.Claim(ctx, store, "holder", id, 0, &pb.HolderEstablished{Id: id})
	if err != nil {
		t.Fatalf("second Claim() error = %v, want nil (convergence, not failure)", err)
	}
	if second {
		t.Fatal("second Claim() won = true, want false: only one caller should win the same expectedSeq")
	}

	events, err := store.Load(ctx, "holder", id)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Load() returned %d events after a lost claim, want 1 (the loser must not have appended anything)", len(events))
	}
}

// A genuine store failure (not a concurrency conflict) must still surface as
// an error rather than being swallowed as a loss.
func TestClaimSurfacesNonConflictErrors(t *testing.T) {
	t.Parallel()

	store := eventstore.NewMemoryStore()
	ctx := context.Background()

	// A negative expectedSeq is rejected by validateWrites, not by the
	// concurrency check — Claim must not mistake this for a lost race.
	won, err := eventstore.Claim(ctx, store, "holder", uniqueID(t), -1, &pb.HolderEstablished{Id: "x"})
	if err == nil {
		t.Fatal("Claim() error = nil, want an error for a negative expectedSeq")
	}
	if errors.Is(err, eventstore.ErrConcurrencyConflict) {
		t.Fatal("Claim() wrapped a validation error as ErrConcurrencyConflict")
	}
	if won {
		t.Fatal("Claim() won = true alongside a non-nil error")
	}
}

func TestMemoryStore(t *testing.T) {
	t.Parallel()

	runStoreContract(t, func(t *testing.T) eventstore.Store {
		t.Helper()
		return eventstore.NewMemoryStore()
	})
}

// runStoreContract exercises the behaviour every Store implementation must
// provide. It runs against MemoryStore always, and against PostgresStore when a
// database is reachable (see postgres_test.go), so the in-memory double can't
// silently drift from the real thing.
func runStoreContract(t *testing.T, newStore func(*testing.T) eventstore.Store) {
	t.Helper()

	ctx := context.Background()

	t.Run("append then load round-trips events in order", func(t *testing.T) {
		store := newStore(t)
		id := uniqueID(t)

		first := &pb.HolderEstablished{Id: id}
		second := &pb.HolderAddedWallet{Id: id, WalletId: "w1", Name: "primary"}

		if err := store.Append(ctx, "holder", id, 0, first, second); err != nil {
			t.Fatalf("Append() error = %v", err)
		}

		events, err := store.Load(ctx, "holder", id)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("Load() returned %d events, want 2", len(events))
		}

		// Sequences are 1-based and contiguous within the stream.
		for i, event := range events {
			wantSeq := int64(i + 1)
			if event.Sequence != wantSeq {
				t.Errorf("events[%d].Sequence = %d, want %d", i, event.Sequence, wantSeq)
			}
			if event.AggregateType != "holder" || event.AggregateID != id {
				t.Errorf("events[%d] = (%q, %q), want (%q, %q)",
					i, event.AggregateType, event.AggregateID, "holder", id)
			}
			if event.OccurredAt.IsZero() {
				t.Errorf("events[%d].OccurredAt is zero", i)
			}
		}

		if got, want := events[0].EventType, eventstore.EventType(first); got != want {
			t.Errorf("events[0].EventType = %q, want %q", got, want)
		}

		decoded, err := events[1].Decode()
		if err != nil {
			t.Fatalf("events[1].Decode() error = %v", err)
		}
		if !proto.Equal(second, decoded) {
			t.Errorf("events[1] decoded to %v, want %v", decoded, second)
		}
	})

	t.Run("load returns nothing for an unknown aggregate", func(t *testing.T) {
		store := newStore(t)

		events, err := store.Load(ctx, "holder", uniqueID(t))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("Load() returned %d events, want 0", len(events))
		}
	})

	t.Run("appending no events is a no-op", func(t *testing.T) {
		store := newStore(t)
		id := uniqueID(t)

		if err := store.Append(ctx, "holder", id, 0); err != nil {
			t.Fatalf("Append() with no events error = %v", err)
		}

		events, err := store.Load(ctx, "holder", id)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("Load() returned %d events, want 0", len(events))
		}
	})

	t.Run("a stale expectedSeq conflicts", func(t *testing.T) {
		store := newStore(t)
		id := uniqueID(t)

		if err := store.Append(ctx, "holder", id, 0, &pb.HolderEstablished{Id: id}); err != nil {
			t.Fatalf("first Append() error = %v", err)
		}

		// Second writer still believes the stream is empty.
		err := store.Append(ctx, "holder", id, 0, &pb.HolderEstablished{Id: id})
		if !errors.Is(err, eventstore.ErrConcurrencyConflict) {
			t.Fatalf("Append() with stale expectedSeq error = %v, want ErrConcurrencyConflict", err)
		}

		events, err := store.Load(ctx, "holder", id)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("Load() returned %d events, want 1 — the losing append must not be recorded", len(events))
		}
	})

	t.Run("appending continues from the current sequence", func(t *testing.T) {
		store := newStore(t)
		id := uniqueID(t)

		if err := store.Append(ctx, "holder", id, 0, &pb.HolderEstablished{Id: id}); err != nil {
			t.Fatalf("first Append() error = %v", err)
		}
		if err := store.Append(ctx, "holder", id, 1, &pb.HolderAddedWallet{Id: id, WalletId: "w1"}); err != nil {
			t.Fatalf("second Append() error = %v", err)
		}

		events, err := store.Load(ctx, "holder", id)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("Load() returned %d events, want 2", len(events))
		}
		if events[1].Sequence != 2 {
			t.Errorf("events[1].Sequence = %d, want 2", events[1].Sequence)
		}
	})

	// A conflicting batch must land atomically or not at all — a half-applied
	// batch would leave the aggregate in a state no command ever produced.
	t.Run("a conflicting batch is applied atomically", func(t *testing.T) {
		store := newStore(t)
		id := uniqueID(t)

		if err := store.Append(ctx, "holder", id, 0, &pb.HolderEstablished{Id: id}); err != nil {
			t.Fatalf("first Append() error = %v", err)
		}

		// Sequence 1 collides; sequence 2 would be free on its own.
		err := store.Append(ctx, "holder", id, 0,
			&pb.HolderEstablished{Id: id},
			&pb.HolderAddedWallet{Id: id, WalletId: "w1"},
		)
		if !errors.Is(err, eventstore.ErrConcurrencyConflict) {
			t.Fatalf("Append() error = %v, want ErrConcurrencyConflict", err)
		}

		events, err := store.Load(ctx, "holder", id)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("Load() returned %d events, want 1 — no part of the conflicting batch may persist", len(events))
		}
	})

	t.Run("streams are isolated by aggregate id and type", func(t *testing.T) {
		store := newStore(t)
		idA, idB := uniqueID(t), uniqueID(t)

		if err := store.Append(ctx, "holder", idA, 0, &pb.HolderEstablished{Id: idA}); err != nil {
			t.Fatalf("Append(idA) error = %v", err)
		}
		if err := store.Append(ctx, "holder", idB, 0, &pb.HolderEstablished{Id: idB}); err != nil {
			t.Fatalf("Append(idB) error = %v", err)
		}
		// Same id under a different aggregate type is a different stream, and
		// so starts its own sequence at 1 rather than conflicting.
		if err := store.Append(ctx, "wallet", idA, 0, &pb.HolderEstablished{Id: idA}); err != nil {
			t.Fatalf("Append(wallet/idA) error = %v", err)
		}

		for _, tc := range []struct{ aggType, id string }{
			{"holder", idA}, {"holder", idB}, {"wallet", idA},
		} {
			events, err := store.Load(ctx, tc.aggType, tc.id)
			if err != nil {
				t.Fatalf("Load(%s/%s) error = %v", tc.aggType, tc.id, err)
			}
			if len(events) != 1 {
				t.Errorf("Load(%s/%s) returned %d events, want 1", tc.aggType, tc.id, len(events))
			}
			if len(events) > 0 && events[0].Sequence != 1 {
				t.Errorf("Load(%s/%s) first sequence = %d, want 1", tc.aggType, tc.id, events[0].Sequence)
			}
		}
	})

	t.Run("an atomic append writes across several streams", func(t *testing.T) {
		store := newStore(t)
		holderID, w1, w2 := uniqueID(t), uniqueID(t), uniqueID(t)

		err := store.AppendAtomic(ctx,
			eventstore.StreamWrite{
				AggregateType: "holder", AggregateID: holderID, ExpectedSeq: 0,
				Events: []proto.Message{
					&pb.HolderEstablished{Id: holderID},
					&pb.HolderAddedWallet{Id: holderID, WalletId: w1},
				},
			},
			eventstore.StreamWrite{
				AggregateType: "wallet", AggregateID: w1, ExpectedSeq: 0,
				Events: []proto.Message{&pb.HolderEstablished{Id: w1}},
			},
			eventstore.StreamWrite{
				AggregateType: "wallet", AggregateID: w2, ExpectedSeq: 0,
				Events: []proto.Message{&pb.HolderEstablished{Id: w2}},
			},
		)
		if err != nil {
			t.Fatalf("AppendAtomic() error = %v", err)
		}

		for _, tc := range []struct {
			aggType, id string
			want        int
		}{{"holder", holderID, 2}, {"wallet", w1, 1}, {"wallet", w2, 1}} {
			events, err := store.Load(ctx, tc.aggType, tc.id)
			if err != nil {
				t.Fatalf("Load(%s/%s) error = %v", tc.aggType, tc.id, err)
			}
			if len(events) != tc.want {
				t.Fatalf("Load(%s/%s) returned %d events, want %d", tc.aggType, tc.id, len(events), tc.want)
			}
			// Each stream sequences from 1 independently of the others.
			for i, event := range events {
				if event.Sequence != int64(i+1) {
					t.Errorf("Load(%s/%s)[%d].Sequence = %d, want %d", tc.aggType, tc.id, i, event.Sequence, i+1)
				}
			}
		}

		// global_seq orders the whole batch in the order the writes were given.
		holderEvents, _ := store.Load(ctx, "holder", holderID)
		w1Events, _ := store.Load(ctx, "wallet", w1)
		if holderEvents[1].GlobalSeq >= w1Events[0].GlobalSeq {
			t.Errorf("global_seq not increasing across the batch: holder[1]=%d then wallet=%d",
				holderEvents[1].GlobalSeq, w1Events[0].GlobalSeq)
		}
	})

	// THE atomicity guarantee. One stream is pre-seeded so its write is certain
	// to collide; every other stream in the batch must be left untouched.
	t.Run("a conflict on any stream rolls back every stream", func(t *testing.T) {
		store := newStore(t)
		holderID, w1, w2 := uniqueID(t), uniqueID(t), uniqueID(t)

		if err := store.Append(ctx, "wallet", w2, 0, &pb.HolderEstablished{Id: w2}); err != nil {
			t.Fatalf("seeding the colliding stream: %v", err)
		}

		err := store.AppendAtomic(ctx,
			eventstore.StreamWrite{
				AggregateType: "holder", AggregateID: holderID, ExpectedSeq: 0,
				Events: []proto.Message{&pb.HolderEstablished{Id: holderID}},
			},
			eventstore.StreamWrite{
				AggregateType: "wallet", AggregateID: w1, ExpectedSeq: 0,
				Events: []proto.Message{&pb.HolderEstablished{Id: w1}},
			},
			eventstore.StreamWrite{
				// Stale: this stream already holds sequence 1.
				AggregateType: "wallet", AggregateID: w2, ExpectedSeq: 0,
				Events: []proto.Message{&pb.HolderEstablished{Id: w2}},
			},
		)
		if !errors.Is(err, eventstore.ErrConcurrencyConflict) {
			t.Fatalf("AppendAtomic() error = %v, want ErrConcurrencyConflict", err)
		}

		for _, tc := range []struct {
			aggType, id string
			want        int
		}{{"holder", holderID, 0}, {"wallet", w1, 0}, {"wallet", w2, 1}} {
			events, err := store.Load(ctx, tc.aggType, tc.id)
			if err != nil {
				t.Fatalf("Load(%s/%s) error = %v", tc.aggType, tc.id, err)
			}
			if len(events) != tc.want {
				t.Errorf("Load(%s/%s) returned %d events, want %d — no part of a conflicting batch may persist",
					tc.aggType, tc.id, len(events), tc.want)
			}
		}
	})

	t.Run("naming one stream twice in an atomic append is rejected", func(t *testing.T) {
		store := newStore(t)
		id := uniqueID(t)

		write := eventstore.StreamWrite{
			AggregateType: "holder", AggregateID: id, ExpectedSeq: 0,
			Events: []proto.Message{&pb.HolderEstablished{Id: id}},
		}
		err := store.AppendAtomic(ctx, write, write)
		if !errors.Is(err, eventstore.ErrDuplicateStream) {
			t.Fatalf("AppendAtomic() error = %v, want ErrDuplicateStream", err)
		}

		events, err := store.Load(ctx, "holder", id)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(events) != 0 {
			t.Errorf("Load() returned %d events, want 0", len(events))
		}
	})

	t.Run("an atomic append with no writes is a no-op", func(t *testing.T) {
		store := newStore(t)
		if err := store.AppendAtomic(ctx); err != nil {
			t.Fatalf("AppendAtomic() with no writes error = %v", err)
		}
	})

	// A write carrying no events inserts nothing, so its ExpectedSeq is
	// unverifiable and must be ignored rather than rejected — otherwise
	// MemoryStore would conflict where PostgresStore silently succeeds.
	t.Run("writes with no events are skipped, not validated", func(t *testing.T) {
		store := newStore(t)
		holderID, empty := uniqueID(t), uniqueID(t)

		err := store.AppendAtomic(ctx,
			eventstore.StreamWrite{
				AggregateType: "holder", AggregateID: holderID, ExpectedSeq: 0,
				Events: []proto.Message{&pb.HolderEstablished{Id: holderID}},
			},
			eventstore.StreamWrite{
				// Nonsense ExpectedSeq, but no events, so it must be ignored.
				AggregateType: "wallet", AggregateID: empty, ExpectedSeq: 99,
			},
		)
		if err != nil {
			t.Fatalf("AppendAtomic() error = %v", err)
		}

		events, err := store.Load(ctx, "holder", holderID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(events) != 1 {
			t.Errorf("Load(holder) returned %d events, want 1 — the other write must still land", len(events))
		}
	})

	t.Run("a stream write without an aggregate identity is rejected", func(t *testing.T) {
		store := newStore(t)

		for _, tc := range []struct {
			name                     string
			aggregateType, aggregate string
		}{
			{"no type", "", uniqueID(t)},
			{"no id", "holder", ""},
		} {
			err := store.AppendAtomic(ctx, eventstore.StreamWrite{
				AggregateType: tc.aggregateType, AggregateID: tc.aggregate, ExpectedSeq: 0,
				Events: []proto.Message{&pb.HolderEstablished{Id: "x"}},
			})
			if err == nil {
				t.Errorf("AppendAtomic() with %s: error = nil, want a rejection", tc.name)
			}
		}
	})

	// Append is a thin delegation to AppendAtomic; this pins that it stays one.
	t.Run("append and atomic append agree for a single stream", func(t *testing.T) {
		store := newStore(t)
		viaAppend, viaAtomic := uniqueID(t), uniqueID(t)

		if err := store.Append(ctx, "holder", viaAppend, 0, &pb.HolderEstablished{Id: viaAppend}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		if err := store.AppendAtomic(ctx, eventstore.StreamWrite{
			AggregateType: "holder", AggregateID: viaAtomic, ExpectedSeq: 0,
			Events: []proto.Message{&pb.HolderEstablished{Id: viaAtomic}},
		}); err != nil {
			t.Fatalf("AppendAtomic() error = %v", err)
		}

		a, _ := store.Load(ctx, "holder", viaAppend)
		b, _ := store.Load(ctx, "holder", viaAtomic)
		if len(a) != 1 || len(b) != 1 {
			t.Fatalf("Load() returned %d and %d events, want 1 each", len(a), len(b))
		}
		if a[0].Sequence != b[0].Sequence || a[0].EventType != b[0].EventType {
			t.Errorf("Append produced (seq=%d, %s) but AppendAtomic produced (seq=%d, %s)",
				a[0].Sequence, a[0].EventType, b[0].Sequence, b[0].EventType)
		}

		// Both paths must reject a stale expectedSeq identically.
		if err := store.Append(ctx, "holder", viaAppend, 0, &pb.HolderEstablished{Id: viaAppend}); !errors.Is(err, eventstore.ErrConcurrencyConflict) {
			t.Errorf("Append() stale error = %v, want ErrConcurrencyConflict", err)
		}
		if err := store.AppendAtomic(ctx, eventstore.StreamWrite{
			AggregateType: "holder", AggregateID: viaAtomic, ExpectedSeq: 0,
			Events: []proto.Message{&pb.HolderEstablished{Id: viaAtomic}},
		}); !errors.Is(err, eventstore.ErrConcurrencyConflict) {
			t.Errorf("AppendAtomic() stale error = %v, want ErrConcurrencyConflict", err)
		}
	})
}

// uniqueID keeps every test case in its own stream. The events table is
// append-only (DELETE is blocked by a trigger), so the Postgres run of this
// suite cannot clean up after itself and relies on never colliding instead —
// including across repeated runs against the same database.
//
// UUIDv7 rather than v4: its leading millisecond timestamp makes the ids sort
// in creation order, which keeps the index on an append-only table healthy and
// lets leftover rows be dated.
func uniqueID(t *testing.T) string {
	t.Helper()
	return uuid.NewV7().String()
}
