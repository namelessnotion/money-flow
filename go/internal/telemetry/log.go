package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// newLogger builds the process logger: JSON lines on cfg.LogOutput, which is
// what `docker logs` and the runbook's grep read, and, when exporting, the
// same records sent over OTLP through lp.
func newLogger(cfg Config, lp log.LoggerProvider) *slog.Logger {
	var h slog.Handler = traceContext{slog.NewJSONHandler(cfg.LogOutput, &slog.HandlerOptions{Level: cfg.LogLevel}).
		WithAttrs([]slog.Attr{slog.String("service", cfg.ServiceName)})}
	if lp != nil {
		// The bridge records trace context natively and the resource names the
		// service, so it gets neither of the JSON handler's additions.
		exported := atLeast{otelslog.NewHandler(scope, otelslog.WithLoggerProvider(lp)), cfg.LogLevel}
		h = slog.NewMultiHandler(h, exported)
	}
	return slog.New(h)
}

// traceContext stamps the active span's trace and span ids on every record
// logged with a context that has one.
type traceContext struct{ slog.Handler }

func (h traceContext) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceContext) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceContext{h.Handler.WithAttrs(attrs)}
}

func (h traceContext) WithGroup(name string) slog.Handler {
	return traceContext{h.Handler.WithGroup(name)}
}

// atLeast holds a handler with no level of its own to the process's level, so
// what is exported matches what is printed.
type atLeast struct {
	slog.Handler
	level slog.Leveler
}

func (h atLeast) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level.Level() && h.Handler.Enabled(ctx, level)
}

func (h atLeast) WithAttrs(attrs []slog.Attr) slog.Handler {
	return atLeast{h.Handler.WithAttrs(attrs), h.level}
}

func (h atLeast) WithGroup(name string) slog.Handler {
	return atLeast{h.Handler.WithGroup(name), h.level}
}
