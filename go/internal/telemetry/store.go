package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

// Store outcomes beyond ok and error. Both are answers the store gives when
// working correctly: a conflict is a lost optimistic-concurrency race, and the
// conflict rate on a stream is how contention on it shows up.
const (
	outcomeConflict        = "conflict"
	outcomeDuplicateStream = "duplicate_stream"
)

// Store is an eventstore.Store that traces and times every call.
//
// It wraps explicitly rather than by embedding, so a method added to
// eventstore.Store fails to compile here instead of going unmeasured.
type Store struct {
	inner    eventstore.Store
	tracer   trace.Tracer
	duration metric.Float64Histogram
	appended metric.Int64Counter
}

var _ eventstore.Store = (*Store)(nil)

func NewStore(inner eventstore.Store, p Providers) *Store {
	b := instruments{meter: p.meter()}
	s := &Store{
		inner:  inner,
		tracer: p.tracer(),
		duration: b.duration("money_flow.eventstore.operation.duration",
			"Time for one event store call, by operation and outcome."),
		appended: b.counter("money_flow.eventstore.events.appended", "{event}",
			"Events durably appended to the log, by aggregate and event type."),
	}
	b.mustBuild()
	return s
}

func (s *Store) Load(ctx context.Context, aggregateType, aggregateID string) ([]eventstore.Event, error) {
	ctx, done := s.start(ctx, "load", keyAggregateType.String(aggregateType), keyAggregateID.String(aggregateID))
	events, err := s.inner.Load(ctx, aggregateType, aggregateID)
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("money_flow.eventstore.events", len(events)))
	done(err, keyAggregateType.String(aggregateType))
	return events, err
}

func (s *Store) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	ctx, done := s.start(ctx, "append",
		keyAggregateType.String(aggregateType),
		keyAggregateID.String(aggregateID),
		attribute.Int64("money_flow.eventstore.expected_seq", expectedSeq),
		attribute.Int("money_flow.eventstore.events", len(events)),
	)
	err := s.inner.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	if err == nil {
		s.countAppended(ctx, aggregateType, events)
	}
	done(err, keyAggregateType.String(aggregateType))
	return err
}

// AppendAtomic is recorded as one operation. Its streams are usually of
// different aggregate types, so the duration carries none; the appended count
// is still attributed stream by stream.
func (s *Store) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	ctx, done := s.start(ctx, "append_atomic", attribute.Int("money_flow.eventstore.streams", len(writes)))
	err := s.inner.AppendAtomic(ctx, writes...)
	if err == nil {
		for _, w := range writes {
			s.countAppended(ctx, w.AggregateType, w.Events)
		}
	}
	done(err)
	return err
}

func (s *Store) Now(ctx context.Context) (time.Time, error) {
	ctx, done := s.start(ctx, "now")
	now, err := s.inner.Now(ctx)
	done(err)
	return now, err
}

func (s *Store) countAppended(ctx context.Context, aggregateType string, events []proto.Message) {
	for _, e := range events {
		s.appended.Add(ctx, 1, metric.WithAttributes(
			keyAggregateType.String(aggregateType),
			keyEventType.String(eventstore.EventType(e)),
		))
	}
}

// start opens the span for one store operation and returns the function that
// closes it. done takes the metric attributes separately from the span's, so
// the aggregate id, which is unbounded, never becomes a metric dimension.
func (s *Store) start(ctx context.Context, op string, attrs ...attribute.KeyValue) (context.Context, func(err error, metricAttrs ...attribute.KeyValue)) {
	began := time.Now()
	ctx, span := s.tracer.Start(ctx, "eventstore."+op,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	return ctx, func(err error, metricAttrs ...attribute.KeyValue) {
		outcome := storeOutcome(err)
		finish(span, outcome, err)
		span.End()

		metricAttrs = append(metricAttrs, keyOperation.String(op), keyOutcome.String(outcome))
		s.duration.Record(ctx, time.Since(began).Seconds(), metric.WithAttributes(metricAttrs...))
	}
}

func storeOutcome(err error) string {
	switch {
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		return outcomeConflict
	case errors.Is(err, eventstore.ErrDuplicateStream):
		return outcomeDuplicateStream
	default:
		return baseOutcome(err)
	}
}
