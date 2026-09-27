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

	// Export sends traces, metrics and logs over OTLP/HTTP. Where to, and
	// with what headers, the exporters read from the standard
	// OTEL_EXPORTER_OTLP_* variables themselves.
	Export bool

	LogLevel  slog.Level
	LogOutput io.Writer
}

// ConfigFromEnv reads the standard OpenTelemetry switches plus LOG_LEVEL.
//
// Export is on exactly when an OTLP endpoint is configured and the SDK has not
// been disabled. With no collector to send to, a binary that tried would only
// log a stream of export failures, so no endpoint means no export, and tests
// and CI need no configuration to stay quiet.
func ConfigFromEnv(serviceName string, getenv func(string) string) (Config, error) {
	cfg := Config{ServiceName: serviceName, LogOutput: os.Stdout}

	endpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "" ||
		getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != ""
	cfg.Export = endpoint && !strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true")

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
// With Export off, the providers are no-ops and only the JSON log is written.
// With it on, each signal is batched and sent over OTLP/HTTP, and Shutdown
// flushes whatever is still buffered.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	if !cfg.Export {
		return &Telemetry{Providers: Noop(), Logger: newLogger(cfg, nil)}, nil
	}

	res, err := newResource(ctx, cfg.ServiceName)
	if err != nil {
		return nil, err
	}
	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
	}
	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
	}
	logExporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: log exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExporter), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)), sdkmetric.WithResource(res))
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)), sdklog.WithResource(res))

	t := &Telemetry{
		Providers: Providers{
			Tracer:     tp,
			Meter:      mp,
			Propagator: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
		},
		Logger: newLogger(cfg, lp),
		// Traces and logs before metrics, so the metric provider's final
		// collection includes anything the others recorded while flushing.
		shutdown: []func(context.Context) error{tp.Shutdown, lp.Shutdown, mp.Shutdown},
	}

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: runtime metrics: %w", err), t.Shutdown(ctx))
	}
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
