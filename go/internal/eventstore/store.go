// Package eventstore is the append-only, immutable log of Domain Events for
// every aggregate (Holder, Wallet, Token, Transfer, Operation). It is the
// source of truth for intent; TigerBeetle remains the source of truth for
// account balances.
package eventstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// ErrConcurrencyConflict is returned by Append when expectedSeq no longer
// matches the aggregate's actual last sequence — some other writer appended
// to the same stream first. Callers should reload the aggregate and retry
// the command against current state.
var ErrConcurrencyConflict = errors.New("eventstore: concurrent append")

// ErrDuplicateStream is returned when one AppendAtomic call names the same
// stream twice. Each write's sequences are computed independently from its own
// ExpectedSeq, so two writes to one stream would collide — a programming error,
// not a race.
var ErrDuplicateStream = errors.New("eventstore: duplicate stream in atomic append")

// StreamWrite is one aggregate's contribution to an atomic append: Events are
// added to (AggregateType, AggregateID)'s stream, sequenced immediately after
// ExpectedSeq.
type StreamWrite struct {
	AggregateType string
	AggregateID   string
	ExpectedSeq   int64
	Events        []proto.Message
}

// Event is one row of the log: a Domain Event that has already happened,
// exactly as recorded — never mutated after the fact.
type Event struct {
	GlobalSeq     int64
	AggregateType string
	AggregateID   string
	Sequence      int64
	EventType     string
	Payload       []byte
	OccurredAt    time.Time
}

// Decode unmarshals the event payload into a fresh instance of its proto
// message type, looked up by EventType in the global registry. The event's
// generated Go package must be linked into the binary (a plain import is
// enough) so its message types are registered.
func (e Event) Decode() (proto.Message, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(e.EventType))
	if err != nil {
		return nil, fmt.Errorf("eventstore: unknown event type %q: %w", e.EventType, err)
	}
	msg := mt.New().Interface()
	if err := proto.Unmarshal(e.Payload, msg); err != nil {
		return nil, fmt.Errorf("eventstore: unmarshal %q: %w", e.EventType, err)
	}
	return msg, nil
}

// Store appends to and loads from aggregates' event streams. Implementations
// must be safe for concurrent use.
type Store interface {
	// Append writes events to (aggregateType, aggregateID)'s stream as a
	// single atomic batch, sequenced immediately after expectedSeq (0 for a
	// brand new aggregate). It fails with ErrConcurrencyConflict if the
	// caller's view of expectedSeq is stale.
	Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error

	// AppendAtomic writes to several aggregates' streams in one all-or-nothing
	// commit. Each write is validated against its own ExpectedSeq exactly as
	// Append does, so no aggregate's optimistic-concurrency invariant is
	// weakened; the only added guarantee is that either every write lands or
	// none does.
	//
	// This is deliberately narrow. It exists so aggregates that are *created
	// together* — a Holder and the Wallets provisioned with it — cannot end up
	// half-created. It is not a licence to mutate independently-lived
	// aggregates in one transaction: a decision on aggregate A that depends on
	// aggregate B's state belongs in a process manager, not here. It is also
	// only implementable while every stream shares one database.
	//
	// Writes carrying no events are ignored, including their ExpectedSeq.
	AppendAtomic(ctx context.Context, writes ...StreamWrite) error

	// Load returns every event in (aggregateType, aggregateID)'s stream,
	// oldest first. An aggregate that doesn't exist yet returns (nil, nil).
	Load(ctx context.Context, aggregateType, aggregateID string) ([]Event, error)

	// Now returns this store's own notion of the current time, on the same
	// clock Event.OccurredAt is stamped with. Callers reasoning about an
	// event's age (go/docs/adr/0005's stale-claim check, chiefly) must
	// compare against this rather than time.Now(): PostgresStore's
	// occurred_at is set server-side by Postgres's own now(), which can
	// drift from the Go process's clock, and a caller comparing across the
	// two clocks can find every event already "stale" the instant it's
	// written, or never stale at all, depending on the direction of drift.
	Now(ctx context.Context) (time.Time, error)
}

// liveWrites drops writes that would insert nothing. They are filtered before
// validation on purpose: PostgresStore cannot check the ExpectedSeq of a stream
// it inserts no rows into, so validating one in MemoryStore would invent a
// divergence between the two implementations.
func liveWrites(writes []StreamWrite) []StreamWrite {
	live := make([]StreamWrite, 0, len(writes))
	for _, w := range writes {
		if len(w.Events) > 0 {
			live = append(live, w)
		}
	}
	return live
}

// validateWrites rejects malformed batches identically in every implementation,
// so a batch that fails in memory fails the same way against Postgres.
func validateWrites(writes []StreamWrite) error {
	seen := make(map[string]struct{}, len(writes))
	for _, w := range writes {
		if w.AggregateType == "" || w.AggregateID == "" {
			return fmt.Errorf("eventstore: stream write is missing aggregate identity")
		}
		if w.ExpectedSeq < 0 {
			return fmt.Errorf("eventstore: negative expectedSeq for %s/%s", w.AggregateType, w.AggregateID)
		}
		key := streamKey(w.AggregateType, w.AggregateID)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: %s/%s", ErrDuplicateStream, w.AggregateType, w.AggregateID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// EventType returns the fully-qualified proto message name used as the
// event_type column and looked up by Event.Decode.
func EventType(m proto.Message) string {
	return string(m.ProtoReflect().Descriptor().FullName())
}

// Claim attempts to append marker as the very next event in
// (aggregateType, aggregateID)'s stream, at expectedSeq. It is this
// codebase's storage-agnostic primitive for "at most one caller performs the
// following side effect at a time" (go/docs/adr/0005): a caller appends a
// marker before doing anything externally visible (a call to another
// service, a ledger submission), and only the caller whose append wins
// proceeds.
//
// won=false, err=nil means a concurrent caller's marker landed first — the
// caller should treat that as convergence, the same way prepare() treats a
// conflicted AppendAtomic, not as failure. The marker is never read back by
// currentState()-style folds; it exists only to be raced over.
func Claim(ctx context.Context, store Store, aggregateType, aggregateID string, expectedSeq int64, marker proto.Message) (won bool, err error) {
	switch err := store.Append(ctx, aggregateType, aggregateID, expectedSeq, marker); {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrConcurrencyConflict):
		return false, nil
	default:
		return false, err
	}
}
