package telemetry_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
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

// A lost append race reaches the pool as Postgres refusing a sequence that is
// already taken. That is the store answering correctly (ADR 0017's "an answer
// is not a fault"), so no statement span turns red and no database error is
// counted; the collision is still on the span as its SQLSTATE and outcome.
func TestInstrumentPool_ALostAppendRaceIsNotAFault(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	pool := testutil.PoolWith(t, func(cfg *pgxpool.Config) { telemetry.InstrumentPool(cfg, rec.Providers) })
	store := eventstore.NewPostgresStore(pool)
	ctx, parent := rec.Tracer.Tracer("t").Start(context.Background(), "eventstore.append")
	id := uuid.NewV7().String()

	if err := store.Append(ctx, "holder", id, 0, &pb.HolderEstablished{Id: id}); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	err := store.Append(ctx, "holder", id, 0, &pb.HolderEstablished{Id: id})
	parent.End()
	if !errors.Is(err, eventstore.ErrConcurrencyConflict) {
		t.Fatalf("second Append() error = %v, want ErrConcurrencyConflict", err)
	}

	var collided int
	for _, s := range rec.spans.Ended() {
		if s.Parent().TraceID() != parent.SpanContext().TraceID() {
			continue
		}
		if s.Status().Code == codes.Error {
			t.Errorf("span %q status = Error (%s), want Unset", s.Name(), s.Status().Description)
		}
		if hasAttr(s.Attributes(), otelpgx.SQLStateKey.String("23505")) {
			collided++
			if !hasAttr(s.Attributes(), attribute.String("money_flow.outcome", "conflict")) {
				t.Errorf("span %q carries the collision without money_flow.outcome=conflict", s.Name())
			}
		}
	}
	if collided == 0 {
		t.Error("no statement span records the collision's SQLSTATE")
	}
	if n := rec.sum(t, "db.client.operation.errors"); n != 0 {
		t.Errorf("db.client.operation.errors = %d, want 0", n)
	}
}

// Every other database error is still a fault, recorded the way otelpgx
// records it.
func TestInstrumentPool_OtherDatabaseErrorsAreStillFaults(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	pool := testutil.PoolWith(t, func(cfg *pgxpool.Config) { telemetry.InstrumentPool(cfg, rec.Providers) })

	ctx, parent := rec.Tracer.Tracer("t").Start(context.Background(), "caller")
	_, err := pool.Exec(ctx, "SELECT * FROM no_such_table")
	parent.End()
	if err == nil {
		t.Fatal("Exec() error = nil, want an undefined-table error")
	}

	var faulted int
	for _, s := range rec.spans.Ended() {
		if s.Parent().SpanID() == parent.SpanContext().SpanID() && s.Status().Code == codes.Error {
			faulted++
		}
	}
	if faulted != 1 {
		t.Errorf("got %d statement spans with status Error, want 1", faulted)
	}
	if n := rec.sum(t, "db.client.operation.errors"); n != 1 {
		t.Errorf("db.client.operation.errors = %d, want 1", n)
	}
}

func hasAttr(attrs []attribute.KeyValue, want attribute.KeyValue) bool {
	for _, kv := range attrs {
		if kv.Key == want.Key && kv.Value.String() == want.Value.String() {
			return true
		}
	}
	return false
}
