package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/telemetry"
)

func envOf(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestConfigFromEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		env        map[string]string
		wantExport bool
		wantLevel  slog.Level
	}{
		{"nothing configured", nil, false, slog.LevelInfo},
		{"an endpoint turns export on", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://lgtm:4318"}, true, slog.LevelInfo},
		{"the SDK kill switch wins", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://lgtm:4318", "OTEL_SDK_DISABLED": "true",
		}, false, slog.LevelInfo},
		{"a log level", map[string]string{"LOG_LEVEL": "debug"}, false, slog.LevelDebug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := telemetry.ConfigFromEnv("money-flow-test", envOf(tc.env))
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v", err)
			}
			if cfg.Export != tc.wantExport {
				t.Errorf("Export = %v, want %v", cfg.Export, tc.wantExport)
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
		ServiceName: "money-flow-test", Export: true, LogLevel: slog.LevelInfo, LogOutput: &bytes.Buffer{},
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
