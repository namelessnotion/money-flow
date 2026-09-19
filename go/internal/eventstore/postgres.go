package eventstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const uniqueViolation = "23505"

// PostgresStore is a Store backed by the events table (see
// go/db/migrations/00001_create_events.up.sql). Optimistic concurrency is
// enforced entirely by that table's UNIQUE(aggregate_type, aggregate_id,
// sequence) constraint: appends compute each event's sequence number from
// ExpectedSeq up front, so a stale ExpectedSeq collides with rows another
// writer already inserted instead of racing a separate check.
//
// That constraint is also what makes AppendAtomic safe across several streams:
// every stream in the batch is re-validated at INSERT time inside the same
// transaction, so a writer landing between a caller's read and this write turns
// into ErrConcurrencyConflict rather than a lost or interleaved write.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	return s.AppendAtomic(ctx, StreamWrite{
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		ExpectedSeq:   expectedSeq,
		Events:        events,
	})
}

func (s *PostgresStore) AppendAtomic(ctx context.Context, writes ...StreamWrite) error {
	writes = liveWrites(writes)
	if err := validateWrites(writes); err != nil {
		return err
	}
	if len(writes) == 0 {
		return nil
	}

	// Everything is marshalled before the transaction opens, so a bad payload
	// never leaves one dangling.
	batch := &pgx.Batch{}
	rows := 0
	for _, w := range writes {
		for i, evt := range w.Events {
			payload, err := proto.Marshal(evt)
			if err != nil {
				return fmt.Errorf("eventstore: marshal %T: %w", evt, err)
			}
			batch.Queue(
				`INSERT INTO events (aggregate_type, aggregate_id, sequence, event_type, payload)
				 VALUES ($1, $2::uuid, $3, $4, $5)`,
				w.AggregateType, w.AggregateID, w.ExpectedSeq+int64(i)+1, EventType(evt), payload,
			)
			rows++
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("eventstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	br := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return ErrConcurrencyConflict
			}
			return fmt.Errorf("eventstore: insert: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("eventstore: close batch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("eventstore: commit: %w", err)
	}
	return nil
}

func (s *PostgresStore) Load(ctx context.Context, aggregateType, aggregateID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT global_seq, aggregate_type, aggregate_id, sequence, event_type, payload, occurred_at
		 FROM events
		 WHERE aggregate_type = $1 AND aggregate_id = $2::uuid
		 ORDER BY sequence ASC`,
		aggregateType, aggregateID,
	)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.GlobalSeq, &e.AggregateType, &e.AggregateID, &e.Sequence, &e.EventType, &e.Payload, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("eventstore: scan: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Now asks Postgres for its own now() rather than reading the Go process's
// clock, so it stays on the same clock occurred_at (DEFAULT now(), see
// go/db/migrations/00001_create_events.up.sql) was stamped with — the two
// can drift otherwise, and a caller comparing an event's age across them
// would be comparing two different clocks without knowing it.
func (s *PostgresStore) Now(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("eventstore: now: %w", err)
	}
	return now, nil
}
