package telemetry_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/namelessnotion/money_flow/go/internal/telemetry"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// Every statement is a span inside whatever operation issued it, so an
// event-store span breaks down into the round trips it actually made.
func TestInstrumentPool_TracesEachStatementInsideItsCaller(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	pool := testutil.PoolWith(t, func(cfg *pgxpool.Config) { telemetry.InstrumentPool(cfg, rec.Providers) })
	if err := telemetry.RecordPoolStats(pool, rec.Providers); err != nil {
		t.Fatalf("RecordPoolStats() error = %v", err)
	}

	ctx, parent := rec.Tracer.Tracer("t").Start(context.Background(), "eventstore.now")
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query error = %v", err)
	}
	parent.End()

	// The pool's own connectivity check ran a SELECT too, outside any
	// caller, so look for the one in this trace.
	var inside int
	for _, s := range rec.spans.Ended() {
		if s.Name() == "SELECT" && s.Parent().SpanID() == parent.SpanContext().SpanID() {
			inside++
		}
	}
	if inside != 1 {
		t.Errorf("got %d SELECT spans under the caller, want 1", inside)
	}
	if _, found := rec.find(t, "pgxpool.acquired_connections"); !found {
		t.Error("pool statistics were not recorded")
	}
}

// Instrumenting only fills in the tracer; the rest of the configuration the
// binaries set, such as MaxConns, is left exactly as it was.
func TestInstrumentPool_InstallsATracerAndNothingElse(t *testing.T) {
	t.Parallel()
	cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost:5432/db_test")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	cfg.MaxConns = 7

	telemetry.InstrumentPool(cfg, telemetry.Noop())

	if cfg.ConnConfig.Tracer == nil {
		t.Error("ConnConfig.Tracer = nil, want a tracer")
	}
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want 7 left alone", cfg.MaxConns)
	}
}
