package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// source is the part of the event log the tail reads.
type source interface {
	// head is the highest global_seq written so far, 0 for an empty log.
	head(ctx context.Context) (int64, error)
	// first is the global_seq of aggregateID's first event, or an error when
	// it has none.
	first(ctx context.Context, aggregateID string) (int64, error)
	// read returns up to limit events after `after` or at one of `holes`, in
	// global_seq order.
	read(ctx context.Context, after int64, holes []int64, limit int) ([]row, error)
}

// pgSource reads the events table. It is a diagnostic reader of the whole log,
// not a way to load an aggregate, so it lives here rather than on
// eventstore.Store.
type pgSource struct {
	pool *pgxpool.Pool
}

func (s pgSource) head(ctx context.Context) (int64, error) {
	var head int64
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(max(global_seq), 0) FROM events`).Scan(&head); err != nil {
		return 0, fmt.Errorf("read the head of the log: %w", err)
	}
	return head, nil
}

func (s pgSource) first(ctx context.Context, aggregateID string) (int64, error) {
	var first *int64
	if err := s.pool.QueryRow(ctx, `SELECT min(global_seq) FROM events WHERE aggregate_id::text = $1`, aggregateID).
		Scan(&first); err != nil {
		return 0, fmt.Errorf("find %s's first event: %w", aggregateID, err)
	}
	if first == nil {
		return 0, fmt.Errorf("no events for aggregate %s", aggregateID)
	}
	return *first, nil
}

func (s pgSource) read(ctx context.Context, after int64, holes []int64, limit int) ([]row, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT global_seq, aggregate_type, aggregate_id::text, sequence, event_type, payload, occurred_at
		 FROM events
		 WHERE global_seq > $1 OR global_seq = ANY($2)
		 ORDER BY global_seq
		 LIMIT $3`,
		after, holes, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	var events []row
	for rows.Next() {
		var e row
		if err := rows.Scan(&e.GlobalSeq, &e.AggregateType, &e.AggregateID, &e.Sequence, &e.EventType, &e.Payload, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
