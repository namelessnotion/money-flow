package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/telemetry"
)

func envOf(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestConfigFromEnv(t *testing.T) {
	t.Parallel()
	type signals struct{ traces, metrics, logs bool }
	all, none := signals{true, true, true}, signals{}
	for _, tc := range []struct {
		name      string
		env       map[string]string
		want      signals
		wantLevel slog.Level
	}{
		{"nothing configured", nil, none, slog.LevelInfo},
		{"the shared endpoint turns every signal on", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://lgtm:4318"}, all, slog.LevelInfo},
		// A signal with no endpoint of its own would fall back to
		// localhost:4318 and fail on every interval.
		{"a signal's own endpoint turns on only that signal", map[string]string{
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://tempo:4318/v1/traces",
		}, signals{traces: true}, slog.LevelInfo},
		{"the SDK kill switch wins", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://lgtm:4318", "OTEL_SDK_DISABLED": "true",
		}, none, slog.LevelInfo},
		{"http/protobuf, stated, is accepted", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://lgtm:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		}, all, slog.LevelInfo},
		{"a log level", map[string]string{"LOG_LEVEL": "debug"}, none, slog.LevelDebug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := telemetry.ConfigFromEnv("money-flow-test", envOf(tc.env))
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v", err)
			}
			if got := (signals{cfg.Traces, cfg.Metrics, cfg.Logs}); got != tc.want {
				t.Errorf("signals exported = %+v, want %+v", got, tc.want)
			}
			if cfg.LogLevel != tc.wantLevel {
				t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, tc.wantLevel)
			}
			if cfg.ServiceName != "money-flow-test" {
				t.Errorf("ServiceName = %q, want money-flow-test", cfg.ServiceName)
			}
		})
	}
}

// Only OTLP over HTTP is built. A protocol asking for anything else is
// refused at startup, not silently answered with HTTP to a gRPC port.
func TestConfigFromEnv_RefusesAProtocolItDoesNotSpeak(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"} {
		env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://lgtm:4317", key: "grpc"}
		if _, err := telemetry.ConfigFromEnv("money-flow-test", envOf(env)); err == nil {
			t.Errorf("ConfigFromEnv() error = nil, want one for %s=grpc", key)
		}
	}
}

// A misspelt level is refused at startup rather than silently logging at the
// default, which is the kind of setting nobody checks until it matters.
func TestConfigFromEnv_RefusesAnUnknownLogLevel(t *testing.T) {
	t.Parallel()
	if _, err := telemetry.ConfigFromEnv("money-flow-test", envOf(map[string]string{"LOG_LEVEL": "loud"})); err == nil {
		t.Fatal("ConfigFromEnv() error = nil, want one for LOG_LEVEL=loud")
	}
}

// Without an endpoint nothing is exported, but the process still logs, as
// JSON, naming the service each line came from.
func TestSetup_WithoutExportLogsJSONAndExportsNothing(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName: "money-flow-test", LogLevel: slog.LevelInfo, LogOutput: &out,
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}

	tel.Logger.Info("started", slog.String("addr", ":8080"))
	tel.Logger.Debug("below the level")

	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("log output %q is not one JSON line: %v", out.String(), err)
	}
	if line["msg"] != "started" || line["addr"] != ":8080" || line["service"] != "money-flow-test" {
		t.Errorf("log line = %v, want msg, addr and service", line)
	}
	if _, span := tel.Tracer.Tracer("t").Start(context.Background(), "s"); span.SpanContext().IsValid() {
		t.Error("a span was recorded with export off")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}
}

// otlpSink is an OTLP/HTTP collector that only remembers which signal paths
// it was sent something on.
type otlpSink struct {
	mu    sync.Mutex
	paths map[string]bool
}

func (s *otlpSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths[r.URL.Path] = true
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// With an endpoint, all three signals reach it by the time Shutdown returns:
// a process that exits straight after Shutdown loses nothing it recorded.
// Not parallel: the OTLP exporters read their endpoint from the environment.
func TestSetup_WithExportDeliversEverySignalByShutdown(t *testing.T) {
	sink := &otlpSink{paths: map[string]bool{}}
	collector := httptest.NewServer(sink)
	t.Cleanup(collector.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)

	ctx := context.Background()
	tel, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName: "money-flow-test", Traces: true, Metrics: true, Logs: true,
		LogLevel: slog.LevelInfo, LogOutput: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	ctx, span := tel.Tracer.Tracer("t").Start(ctx, "work")
	tel.Logger.InfoContext(ctx, "working")
	span.End()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, path := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		if !sink.paths[path] {
			t.Errorf("nothing was exported to %s; got %v", path, sink.paths)
		}
	}
}

// A process exporting only traces sends nothing anywhere else, rather than
// failing every interval against a default endpoint for the rest.
// Not parallel: the OTLP exporters read their endpoint from the environment.
func TestSetup_ExportsOnlyTheSignalsConfigured(t *testing.T) {
	sink := &otlpSink{paths: map[string]bool{}}
	collector := httptest.NewServer(sink)
	t.Cleanup(collector.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", collector.URL+"/v1/traces")

	ctx := context.Background()
	cfg, err := telemetry.ConfigFromEnv("money-flow-test", os.Getenv)
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	cfg.LogOutput = &bytes.Buffer{}
	tel, err := telemetry.Setup(ctx, cfg)
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	ctx, span := tel.Tracer.Tracer("t").Start(ctx, "work")
	tel.Logger.InfoContext(ctx, "working")
	span.End()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.paths) != 1 || !sink.paths["/v1/traces"] {
		t.Errorf("exported to %v, want /v1/traces only", sink.paths)
	}
}
