package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// target is one aggregate to drive.
type target struct {
	aggregateType string
	aggregateID   string
}

func (t target) String() string { return t.aggregateType + " " + t.aggregateID }

// catalogue finds aggregates by reading the whole events table. Like
// cmd/events' own pgSource, it is a diagnostic reader of the log rather than a
// way to load an aggregate, so it lives here rather than on eventstore.Store —
// which deliberately offers no way to ask "what streams exist?".
type catalogue struct {
	pool *pgxpool.Pool
}

// typesOf reports which aggregate types have a stream under id. Normally
// exactly one; see resolveTargets for why the caller cares about the others.
func (c catalogue) typesOf(ctx context.Context, id string) ([]string, error) {
	rows, err := c.pool.Query(ctx,
		`SELECT DISTINCT aggregate_type FROM events WHERE aggregate_id = $1::uuid ORDER BY aggregate_type`, id)
	if err != nil {
		return nil, fmt.Errorf("look up aggregate %s: %w", id, err)
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("look up aggregate %s: %w", id, err)
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

// sagaStreams lists every Transaction and Transfer stream in the log that has
// not finished — none of its events is one the aggregate names as terminal.
// Terminals are absorbing, so a stream holding one can be skipped without
// folding it, whatever noise was recorded after it; on a long-lived log that is
// almost every stream. Whether what is left is actually still going anywhere
// is a fold of each aggregate's own stream, so it stays driver.inFlight's
// question — the database narrows what exists, and only the aggregates know
// what it means.
func (c catalogue) sagaStreams(ctx context.Context) ([]target, error) {
	terminals := append(transaction.TerminalEventTypes(), transfer.TerminalEventTypes()...)
	rows, err := c.pool.Query(ctx,
		`SELECT aggregate_type, aggregate_id::text FROM events
		  WHERE aggregate_type = ANY($1)
		  GROUP BY aggregate_type, aggregate_id
		 HAVING NOT bool_or(event_type = ANY($2))
		  ORDER BY aggregate_type, aggregate_id`,
		[]string{transaction.AggregateType, transfer.AggregateType}, terminals)
	if err != nil {
		return nil, fmt.Errorf("list aggregates: %w", err)
	}
	defer rows.Close()

	var candidates []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.aggregateType, &t.aggregateID); err != nil {
			return nil, fmt.Errorf("list aggregates: %w", err)
		}
		candidates = append(candidates, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list aggregates: %w", err)
	}
	return candidates, nil
}
