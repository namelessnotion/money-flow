// Package telemetry is how the Go binaries report what they are doing: traces,
// metrics and structured logs, exported over OTLP (go/docs/adr/0017).
//
// It instruments at the ports and nowhere else. The event store, the ledger,
// the Twirp surface and the saga handler are each wrapped by a decorator here,
// the same shape internal/tlatrace already uses, so the domain packages
// (transfer, transaction, holder, wallet, token) never import OpenTelemetry and
// never have to decide what is worth measuring.
//
// Nothing here reads or writes OpenTelemetry's global providers. Every
// decorator takes its Providers explicitly, which is what lets a test observe
// one decorator's spans without racing every other parallel test for a
// process-wide tracer.
package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// scope names the instrumentation library on every span and metric this
// package records.
const scope = "github.com/namelessnotion/money_flow/go/internal/telemetry"

// Providers is where instrumented code sends what it records.
type Providers struct {
	Tracer     trace.TracerProvider
	Meter      metric.MeterProvider
	Propagator propagation.TextMapPropagator
}

// Noop records nothing. It is what a binary runs with when no OTLP endpoint
// is configured, and what a test passes when it is not about telemetry.
func Noop() Providers {
	return Providers{
		Tracer:     tracenoop.NewTracerProvider(),
		Meter:      metricnoop.NewMeterProvider(),
		Propagator: propagation.NewCompositeTextMapPropagator(),
	}
}

func (p Providers) tracer() trace.Tracer { return p.Tracer.Tracer(scope) }
func (p Providers) meter() metric.Meter  { return p.Meter.Meter(scope) }

// Attribute keys shared by more than one decorator. Aggregate identity is the
// thread that ties an RPC, the appends it made, and the saga steps those
// appends later triggered into one story, because trace context itself does
// not survive the CDC hop (go/docs/adr/0017).
const (
	keyAggregateType = attribute.Key("money_flow.aggregate_type")
	keyAggregateID   = attribute.Key("money_flow.aggregate_id")
	keyEventType     = attribute.Key("money_flow.event_type")
	keyOutcome       = attribute.Key("money_flow.outcome")
	keyOperation     = attribute.Key("money_flow.operation")
)

// Outcomes shared by the decorators. An outcome is what the caller was told;
// only outcomeError is a failure of the system rather than an answer from it.
const (
	outcomeOK    = "ok"
	outcomeError = "error"
)

// durationBuckets suit calls that are usually a few milliseconds and
// occasionally a second or more: a store round trip, a ledger batch, one
// saga step. They are in seconds, the unit every duration here is recorded in.
var durationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// instruments builds a decorator's instruments, remembering the first error.
//
// Instrument names are constants in this package, so an error here is a
// programming mistake that every test constructing the decorator over a real
// SDK catches. Constructors panic on it rather than returning it, which keeps
// wiring in cmd/ to one line per decorator.
type instruments struct {
	meter metric.Meter
	err   error
}

func (b *instruments) duration(name, description string) metric.Float64Histogram {
	h, err := b.meter.Float64Histogram(name,
		metric.WithUnit("s"),
		metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(durationBuckets...),
	)
	b.keep(err)
	return h
}

func (b *instruments) counter(name, unit, description string) metric.Int64Counter {
	c, err := b.meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(description))
	b.keep(err)
	return c
}

func (b *instruments) keep(err error) {
	if b.err == nil {
		b.err = err
	}
}

func (b *instruments) mustBuild() {
	if b.err != nil {
		panic("telemetry: " + b.err.Error())
	}
}
