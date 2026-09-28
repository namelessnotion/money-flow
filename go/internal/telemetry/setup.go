package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config is what a binary decides about its own telemetry at startup.
type Config struct {
	// ServiceName identifies the binary, e.g. money-flow-server.
	// OTEL_SERVICE_NAME overrides it on what is exported.
	ServiceName string

	// Traces, Metrics and Logs each send that signal over OTLP/HTTP. Where
	// to, and with what headers, the exporters read from the standard
	// OTEL_EXPORTER_OTLP_* variables themselves.
	Traces, Metrics, Logs bool

	LogLevel  slog.Level
	LogOutput io.Writer
}

func (c Config) exporting() bool { return c.Traces || c.Metrics || c.Logs }

// ConfigFromEnv reads the standard OpenTelemetry switches plus LOG_LEVEL.
//
// A signal is exported exactly when an OTLP endpoint is configured for it,
// shared or its own, and the SDK has not been disabled. With no collector to
// send to, an exporter would fall back to localhost:4318 and log a failure on
// every interval, so a signal with no endpoint is not exported at all, and
// tests and CI need no configuration to stay quiet.
//
// Only OTLP over HTTP (http/protobuf) is built. A protocol variable asking for
// anything else is refused rather than ignored.
func ConfigFromEnv(serviceName string, getenv func(string) string) (Config, error) {
	cfg := Config{ServiceName: serviceName, LogOutput: os.Stdout}

	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	} {
		if protocol := getenv(key); protocol != "" && protocol != "http/protobuf" {
			return Config{}, fmt.Errorf("telemetry: %s=%q: only http/protobuf is supported", key, protocol)
		}
	}

	disabled := strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true")
	shared := getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	exported := func(signal string) bool {
		return !disabled && (shared || getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != "")
	}
	cfg.Traces, cfg.Metrics, cfg.Logs = exported("TRACES"), exported("METRICS"), exported("LOGS")

	if level := getenv("LOG_LEVEL"); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return Config{}, fmt.Errorf("telemetry: LOG_LEVEL: %w", err)
		}
	}
	return cfg, nil
}

// Telemetry is one process's providers and logger, and what flushes them.
type Telemetry struct {
	Providers
	Logger *slog.Logger

	shutdown []func(context.Context) error
}

// Setup builds the providers and logger cfg asks for.
//
// A signal that is not exported gets a no-op provider, and the JSON log is
// written either way. An exported signal is batched and sent over OTLP/HTTP,
// and Shutdown flushes whatever is still buffered.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	t := &Telemetry{Providers: Noop()}
	if !cfg.exporting() {
		t.Logger = newLogger(cfg, nil)
		return t, nil
	}

	res, err := newResource(ctx, cfg.ServiceName)
	if err != nil {
		return nil, err
	}
	// fail flushes whatever was already built, so a half-built Telemetry
	// never leaks a running exporter.
	fail := func(err error) (*Telemetry, error) { return nil, errors.Join(err, t.Shutdown(ctx)) }

	// Built, and so flushed, in this order: traces and logs before metrics,
	// so the metric provider's final collection includes anything the others
	// recorded while flushing.
	if cfg.Traces {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("telemetry: trace exporter: %w", err))
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
		t.Tracer = tp
		t.Propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
		t.shutdown = append(t.shutdown, tp.Shutdown)
	}
	var lp log.LoggerProvider
	if cfg.Logs {
		exporter, err := otlploghttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("telemetry: log exporter: %w", err))
		}
		provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)), sdklog.WithResource(res))
		lp = provider
		t.shutdown = append(t.shutdown, provider.Shutdown)
	}
	if cfg.Metrics {
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("telemetry: metric exporter: %w", err))
		}
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)), sdkmetric.WithResource(res))
		t.Meter = mp
		t.shutdown = append(t.shutdown, mp.Shutdown)
		if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
			return fail(fmt.Errorf("telemetry: runtime metrics: %w", err))
		}
	}
	t.Logger = newLogger(cfg, lp)
	return t, nil
}

// Shutdown flushes and stops every provider. It is safe to call on a
// Telemetry that exports nothing.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, shutdown := range t.shutdown {
		errs = append(errs, shutdown(ctx))
	}
	return errors.Join(errs...)
}

// newResource describes this process on everything it exports. The
// environment is merged last, so OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES override the defaults here.
func newResource(ctx context.Context, serviceName string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{attribute.String("service.name", serviceName)}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		attrs = append(attrs, attribute.String("service.version", info.Main.Version))
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithProcessPID(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}
	return res, nil
}
