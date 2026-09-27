package telemetry

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/namelessnotion/money_flow/go/internal/saga"
)

type sagaHandler struct {
	inner    saga.Handler
	tracer   trace.Tracer
	duration metric.Float64Histogram
}

// SagaHandler wraps a saga.Handler so each delivered trigger is one consumer
// span, with every store and ledger call the saga step makes nested inside it.
//
// Each span starts a new trace. The trigger was published by CDC from a
// committed row, and the trace context of the RPC that wrote the row is not
// carried through Postgres and Debezium (go/docs/adr/0017). The trigger's
// aggregate id and global_seq are on the span to link the two by hand.
//
// The span covers one attempt. saga.Consumer retries a failed attempt before
// halting, so a halt is preceded by the failed spans of every attempt.
func SagaHandler(p Providers, inner saga.Handler) saga.Handler {
	b := instruments{meter: p.meter()}
	h := &sagaHandler{
		inner:  inner,
		tracer: p.tracer(),
		duration: b.duration("money_flow.saga.handle.duration",
			"Time to handle one delivered trigger, by aggregate type, event type and outcome."),
	}
	b.mustBuild()
	return h
}

func (h *sagaHandler) Handle(ctx context.Context, t saga.Trigger) error {
	began := time.Now()
	ctx, span := h.tracer.Start(ctx, "saga.handle "+t.AggregateType,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			keyAggregateType.String(t.AggregateType),
			keyAggregateID.String(t.AggregateID),
			keyEventType.String(t.EventType),
			attribute.Int64("money_flow.sequence", t.Sequence),
			attribute.Int64("money_flow.global_seq", t.GlobalSeq),
		),
	)
	defer span.End()

	err := h.inner.Handle(ctx, t)

	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.SetAttributes(keyOutcome.String(outcome))
	h.duration.Record(ctx, time.Since(began).Seconds(), metric.WithAttributes(
		keyAggregateType.String(t.AggregateType),
		keyEventType.String(t.EventType),
		keyOutcome.String(outcome),
	))
	return err
}

// LagFunc measures a consumer group's lag on one topic: for each partition,
// how many messages have been published that the group has not yet committed.
type LagFunc func(ctx context.Context) (map[int]int64, error)

// RegisterConsumerLag reports measure as the money_flow.saga.consumer.lag
// gauge, measured each time metrics are collected. It returns the function
// that stops reporting.
//
// This is the signal to alert on (go/docs/adr/0003, docs/saga-orchestrator.md).
// An event log is legitimately idle for long stretches, so "nothing happened"
// means nothing. Lag that grows while the orchestrator is up means it is not
// keeping up, or not consuming at all.
//
// A failed measurement records no data point rather than a zero, because zero
// would read as "caught up". The failure is logged instead.
func RegisterConsumerLag(p Providers, logger *slog.Logger, group, topic string, measure LagFunc) (unregister func() error) {
	meter := p.meter()
	gauge, err := meter.Int64ObservableGauge("money_flow.saga.consumer.lag",
		metric.WithUnit("{message}"),
		metric.WithDescription("Messages published to a topic that its saga consumer group has not yet committed, by partition."),
	)
	if err != nil {
		panic("telemetry: " + err.Error())
	}
	groupAttr := attribute.String("messaging.consumer.group.name", group)
	topicAttr := attribute.String("messaging.destination.name", topic)

	reg, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		lag, err := measure(ctx)
		if err != nil {
			logger.WarnContext(ctx, "cannot measure consumer lag",
				slog.String("group", group), slog.String("topic", topic), slog.Any("err", err))
			return nil
		}
		for partition, n := range lag {
			o.ObserveInt64(gauge, n, metric.WithAttributes(groupAttr, topicAttr,
				attribute.String("messaging.destination.partition.id", strconv.Itoa(partition))))
		}
		return nil
	}, gauge)
	if err != nil {
		panic("telemetry: " + err.Error())
	}
	return reg.Unregister
}
