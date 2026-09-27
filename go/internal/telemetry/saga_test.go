package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"

	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/telemetry"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

type handlerFunc func(context.Context, saga.Trigger) error

func (f handlerFunc) Handle(ctx context.Context, t saga.Trigger) error { return f(ctx, t) }

func transferTrigger(id string) saga.Trigger {
	return saga.Trigger{
		AggregateType: "transfer", AggregateID: id,
		EventType: "transfer.v1.TransferRequestAccepted", Sequence: 1, GlobalSeq: 42,
	}
}

// Each delivered trigger is one consumer span carrying the trigger's identity,
// and the store and ledger work the saga step does happens inside it.
func TestHandler_EachTriggerIsAConsumerSpanAroundTheSagaStep(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	var inner trace.SpanContext
	h := telemetry.SagaHandler(rec.Providers, handlerFunc(func(ctx context.Context, _ saga.Trigger) error {
		inner = trace.SpanContextFromContext(ctx)
		return nil
	}))
	id := testutil.ID("woken")

	if err := h.Handle(context.Background(), transferTrigger(id)); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	span := rec.span(t, "saga.handle transfer")
	if span.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("span kind = %v, want consumer", span.SpanKind())
	}
	if inner.SpanID() != span.SpanContext().SpanID() {
		t.Error("the saga step did not run inside the handle span")
	}
	wantAttr(t, span, "money_flow.aggregate_type", "transfer")
	wantAttr(t, span, "money_flow.aggregate_id", id)
	wantAttr(t, span, "money_flow.event_type", "transfer.v1.TransferRequestAccepted")
	wantAttr(t, span, "money_flow.sequence", "1")
	wantAttr(t, span, "money_flow.global_seq", "42")
	if n := rec.histogramCount(t, "money_flow.saga.handle.duration",
		attribute.String("money_flow.aggregate_type", "transfer"),
		attribute.String("money_flow.outcome", "ok")); n != 1 {
		t.Errorf("handle duration observations = %d, want 1", n)
	}
}

// A failed attempt is one the consumer will retry, and after its last, halt
// on (go/docs/adr/0003). Each is a failed span, so retries are visible as
// failures before any halt.
func TestHandler_AFailedAttemptIsAnError(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	boom := errors.New("store unreachable")
	h := telemetry.SagaHandler(rec.Providers, handlerFunc(func(context.Context, saga.Trigger) error { return boom }))

	if err := h.Handle(context.Background(), transferTrigger(testutil.ID("stuck"))); !errors.Is(err, boom) {
		t.Fatalf("Handle() error = %v, want the inner error passed through", err)
	}

	span := rec.span(t, "saga.handle transfer")
	if span.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", span.Status().Code)
	}
	if n := rec.histogramCount(t, "money_flow.saga.handle.duration",
		attribute.String("money_flow.outcome", "error")); n != 1 {
		t.Errorf("failed handle observations = %d, want 1", n)
	}
}

// Consumer-group lag is the signal go/docs/adr/0003 says to alert on, because
// an event log is legitimately idle for long stretches and silence means
// nothing. It is reported per partition.
func TestConsumerLag_ReportsEachPartitionsLag(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	unregister := telemetry.RegisterConsumerLag(rec.Providers, slog.New(slog.DiscardHandler),
		"money-flow-saga-transfer", "transfer-events",
		func(context.Context) (map[int]int64, error) { return map[int]int64{0: 3, 1: 0}, nil })
	t.Cleanup(func() { _ = unregister() })

	g, ok := rec.metric(t, "money_flow.saga.consumer.lag").Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatal("consumer lag is not an int64 gauge")
	}
	got := map[string]int64{}
	for _, dp := range g.DataPoints {
		group, _ := dp.Attributes.Value("messaging.consumer.group.name")
		topic, _ := dp.Attributes.Value("messaging.destination.name")
		if group.AsString() != "money-flow-saga-transfer" || topic.AsString() != "transfer-events" {
			t.Errorf("data point attributes = %v, want the group and topic", dp.Attributes.ToSlice())
		}
		partition, _ := dp.Attributes.Value("messaging.destination.partition.id")
		got[partition.AsString()] = dp.Value
	}
	if len(got) != 2 || got["0"] != 3 || got["1"] != 0 {
		t.Errorf("lag by partition = %v, want map[0:3 1:0]", got)
	}
}

// A lag that cannot be measured is reported as missing, never as zero, which
// would read as "caught up", and the reason is logged.
func TestConsumerLag_AnUnmeasurableLagIsMissingNotZero(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	var logs bytes.Buffer
	unregister := telemetry.RegisterConsumerLag(rec.Providers, slog.New(slog.NewTextHandler(&logs, nil)),
		"money-flow-saga-transfer", "transfer-events",
		func(context.Context) (map[int]int64, error) { return nil, errors.New("no broker reachable") })
	t.Cleanup(func() { _ = unregister() })

	if m, found := rec.find(t, "money_flow.saga.consumer.lag"); found {
		if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) > 0 {
			t.Errorf("got %d lag data points, want none", len(g.DataPoints))
		}
	}
	if !strings.Contains(logs.String(), "no broker reachable") {
		t.Errorf("log = %q, want the measurement failure", logs.String())
	}
}
