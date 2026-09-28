package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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

// oneMessage delivers a single trigger and then waits, like an idle topic.
type oneMessage struct {
	msg       saga.Message
	delivered bool
}

func (r *oneMessage) Fetch(ctx context.Context) (saga.Message, error) {
	if r.delivered {
		<-ctx.Done()
		return saga.Message{}, ctx.Err()
	}
	r.delivered = true
	return r.msg, nil
}

func (r *oneMessage) Commit(context.Context, ...saga.Message) error { return nil }

// The halt line an operator starts from carries the trace of the attempt that
// failed for good, which is the whole point of stamping trace ids on logs
// (docs/saga-orchestrator.md, recovering from a halt, step 1).
func TestSagaAttempts_TheHaltLineCarriesTheFailedAttemptsTrace(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	var out bytes.Buffer
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceName: "money-flow-test", LogLevel: slog.LevelInfo, LogOutput: &out,
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	id := testutil.ID("halting")
	reader := &oneMessage{msg: saga.Message{
		Topic: "transfer-events", Key: []byte(id),
		Value: []byte(`{"aggregate_type":"transfer","event_type":"transfer.v1.TransferRequestAccepted","sequence":1,"global_seq":1}`),
	}}
	c := saga.NewConsumer(reader, handlerFunc(func(context.Context, saga.Trigger) error { return errors.New("invariant broken") }),
		saga.WithAttempts(2), saga.WithBackoff(0), saga.WithLogger(tel.Logger),
		saga.WithAttemptObserver(telemetry.SagaAttempts(rec.Providers)))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var halt *saga.HaltError
	if err := c.Run(ctx); !errors.As(err, &halt) {
		t.Fatalf("Run() error = %v, want a *saga.HaltError", err)
	}

	var attempts []sdktrace.ReadOnlySpan
	for _, s := range rec.spans.Ended() {
		if s.Name() == "saga.handle transfer" {
			attempts = append(attempts, s)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("got %d attempt spans, want 2", len(attempts))
	}
	wantAttr(t, attempts[1], "money_flow.saga.attempt", "2")
	var haltLine map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var l map[string]any
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if strings.Contains(l["msg"].(string), "HALTED") {
			haltLine = l
		}
	}
	if haltLine == nil {
		t.Fatalf("no HALTED line in %q", out.String())
	}
	if got, want := haltLine["trace_id"], attempts[1].SpanContext().TraceID().String(); got != want {
		t.Errorf("halt trace_id = %v, want the last attempt's %s", got, want)
	}
}

// A shutdown that lands mid-step cancels the step. That is the process being
// stopped, not the step failing, and it must not add error points to the
// series an alert watches on every deploy.
func TestHandler_ACancelledStepIsCanceledNotAnError(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	h := telemetry.SagaHandler(rec.Providers, handlerFunc(func(ctx context.Context, _ saga.Trigger) error {
		return fmt.Errorf("load: %w", context.Canceled)
	}))

	_ = h.Handle(context.Background(), transferTrigger(testutil.ID("stopped")))

	span := rec.span(t, "saga.handle transfer")
	wantAttr(t, span, "money_flow.outcome", "canceled")
	if span.Status().Code != codes.Unset {
		t.Errorf("status = %v, want Unset", span.Status().Code)
	}
	if n := rec.histogramCount(t, "money_flow.saga.handle.duration",
		attribute.String("money_flow.outcome", "error")); n != 0 {
		t.Errorf("error observations = %d, want 0", n)
	}
}
