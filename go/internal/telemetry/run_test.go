package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/telemetry"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// A binary's run that fails is reported through its own logger, and the
// failure is handed back so main can exit non-zero.
func TestRunWith_LogsAndReturnsTheRunsError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	boom := errors.New("ping: connection refused")

	err := telemetry.RunWith(context.Background(), telemetry.Config{
		ServiceName: "money-flow-test", LogLevel: slog.LevelInfo, LogOutput: &out,
	}, func(context.Context, *telemetry.Telemetry) error { return boom })

	if !errors.Is(err, boom) {
		t.Fatalf("RunWith() error = %v, want the run's error", err)
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &line); err != nil {
		t.Fatalf("log output %q is not one JSON line: %v", out.String(), err)
	}
	if line["msg"] != "stopped" || line["level"] != "ERROR" || !strings.Contains(line["err"].(string), "connection refused") {
		t.Errorf("log line = %v, want an ERROR stopped line carrying the error", line)
	}
}

func TestRunWith_ACleanStopIsLoggedAsInfo(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	err := telemetry.RunWith(context.Background(), telemetry.Config{
		ServiceName: "money-flow-test", LogLevel: slog.LevelInfo, LogOutput: &out,
	}, func(context.Context, *telemetry.Telemetry) error { return nil })

	if err != nil {
		t.Fatalf("RunWith() error = %v", err)
	}
	if !strings.Contains(out.String(), `"level":"INFO","msg":"stopped"`) {
		t.Errorf("log = %q, want an INFO stopped line", out.String())
	}
}

// What a run recorded, up to and including the line saying it stopped, is
// exported before RunWith returns, because main exits straight after it.
// Not parallel: the OTLP exporters read their endpoint from the environment.
func TestRunWith_FlushesBeforeReturning(t *testing.T) {
	sink := &otlpSink{paths: map[string]bool{}}
	collector := httptest.NewServer(sink)
	t.Cleanup(collector.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)

	err := telemetry.RunWith(context.Background(), telemetry.Config{
		ServiceName: "money-flow-test", Traces: true, Logs: true, LogLevel: slog.LevelInfo, LogOutput: &bytes.Buffer{},
	}, func(ctx context.Context, tel *telemetry.Telemetry) error {
		_, span := tel.Tracer.Tracer("t").Start(ctx, "work")
		span.End()
		return nil
	})
	if err != nil {
		t.Fatalf("RunWith() error = %v", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.paths["/v1/traces"] || !sink.paths["/v1/logs"] {
		t.Errorf("exported to %v, want traces and logs flushed", sink.paths)
	}
}

// Orchestrator is the one place the orchestrator's store and ledger are
// instrumented, for every binary that drives sagas: what it does lands in
// the saga step's trace.
func TestOrchestrator_DrivesThroughTheInstrumentedStore(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	o := telemetry.Orchestrator(rec.Providers, eventstore.NewMemoryStore(), ledger.NewFakeClient())

	_ = o.Handle(context.Background(), saga.Trigger{AggregateType: "transfer", AggregateID: testutil.ID("absent")})

	var loads int
	for _, s := range rec.spans.Ended() {
		if s.Name() == "eventstore.load" {
			loads++
		}
	}
	if loads == 0 {
		t.Error("no eventstore.load span: the orchestrator's store is not instrumented")
	}
}
