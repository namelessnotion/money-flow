package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/telemetry"
)

// A log line written inside a span carries its trace and span ids, which is
// what takes an operator from a halt in the logs to the trace of the attempt
// that failed.
func TestLogger_StampsTheActiveSpanOnEachLine(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	var out bytes.Buffer
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName: "money-flow-test", LogLevel: slog.LevelInfo, LogOutput: &out,
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	ctx, span := rec.Tracer.Tracer("t").Start(context.Background(), "work")
	defer span.End()

	tel.Logger.InfoContext(ctx, "inside")

	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("log output %q is not JSON: %v", out.String(), err)
	}
	if got, want := line["trace_id"], span.SpanContext().TraceID().String(); got != want {
		t.Errorf("trace_id = %v, want %s", got, want)
	}
	if got, want := line["span_id"], span.SpanContext().SpanID().String(); got != want {
		t.Errorf("span_id = %v, want %s", got, want)
	}
}

func TestLogger_OmitsTraceIDsOutsideASpan(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName: "money-flow-test", LogLevel: slog.LevelInfo, LogOutput: &out,
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}

	tel.Logger.InfoContext(context.Background(), "outside")

	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("log output %q is not JSON: %v", out.String(), err)
	}
	if _, found := line["trace_id"]; found {
		t.Errorf("log line %v has a trace_id with no span", line)
	}
}
