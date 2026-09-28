package telemetry

import (
	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InstrumentPool makes every statement run on a pool built from cfg a span,
// nested inside the store operation that issued it. Only the statement text
// is recorded, never its parameters, which carry amounts and ids.
func InstrumentPool(cfg *pgxpool.Config, p Providers) {
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithTracerProvider(p.Tracer),
		otelpgx.WithMeterProvider(p.Meter),
	)
}

// RecordPoolStats reports pool's saturation (connections acquired, idle, and
// the time spent waiting for one) as gauges. A pool at its MaxConns with
// callers waiting is the first thing to rule out when store latency climbs.
func RecordPoolStats(pool *pgxpool.Pool, p Providers) error {
	return otelpgx.RecordStats(pool, otelpgx.WithStatsMeterProvider(p.Meter))
}
