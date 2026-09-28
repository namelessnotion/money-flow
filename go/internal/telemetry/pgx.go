package telemetry

import (
	"context"
	"errors"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

// InstrumentPool makes every statement run on a pool built from cfg a span,
// nested inside the store operation that issued it. Only the statement text
// is recorded, never its parameters, which carry amounts and ids.
func InstrumentPool(cfg *pgxpool.Config, p Providers) {
	cfg.ConnConfig.Tracer = pgxTracer{otelpgx.NewTracer(
		otelpgx.WithTracerProvider(p.Tracer),
		otelpgx.WithMeterProvider(p.Meter),
	)}
}

// pgxTracer is otelpgx's tracer, except that a lost append race is an answer,
// not a fault. The events table's unique constraint is how the store detects
// a stale expected sequence, so Postgres reports every lost race as an error.
// otelpgx would mark the statement span Error and count a database error for
// each one, turning traces red on ordinary contention. Here the collision is
// recorded on the span as money_flow.outcome=conflict with its SQLSTATE, and
// otelpgx sees the statement as having succeeded.
//
// It embeds rather than wrapping explicitly, unlike the store and ledger
// decorators: every other hook otelpgx implements (connect, prepare, copy,
// acquire) should pass through untouched, including any added upstream.
type pgxTracer struct {
	*otelpgx.Tracer
}

func (t pgxTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	data.Err = answerCollision(ctx, data.Err)
	t.Tracer.TraceQueryEnd(ctx, conn, data)
}

func (t pgxTracer) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	data.Err = answerCollision(ctx, data.Err)
	t.Tracer.TraceBatchQuery(ctx, conn, data)
}

func (t pgxTracer) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchEndData) {
	data.Err = answerCollision(ctx, data.Err)
	t.Tracer.TraceBatchEnd(ctx, conn, data)
}

// answerCollision records a sequence collision on the span ctx carries and
// returns nil in its place. Any other error is returned unchanged, for otelpgx
// to record as the fault it is.
func answerCollision(ctx context.Context, err error) error {
	if !eventstore.IsSequenceCollision(err) {
		return err
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(keyOutcome.String(outcomeConflict))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		span.SetAttributes(otelpgx.SQLStateKey.String(pgErr.Code))
	}
	return nil
}

// RecordPoolStats reports pool's saturation (connections acquired, idle, and
// the time spent waiting for one) as gauges. A pool at its MaxConns with
// callers waiting is the first thing to rule out when store latency climbs.
func RecordPoolStats(pool *pgxpool.Pool, p Providers) error {
	return otelpgx.RecordStats(pool, otelpgx.WithStatsMeterProvider(p.Meter))
}
