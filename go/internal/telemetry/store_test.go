package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/telemetry"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

const holderType = "holder"

// A landed append is one span and one duration observation, and each event it
// wrote is counted under its type.
func TestStore_ALandedAppendIsTracedTimedAndCounted(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	store := telemetry.NewStore(eventstore.NewMemoryStore(), rec.Providers)
	id := testutil.ID("landed")

	if err := store.Append(context.Background(), holderType, id, 0, &pb.HolderEstablished{Id: id}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	span := rec.span(t, "eventstore.append")
	wantAttr(t, span, "money_flow.aggregate_type", holderType)
	wantAttr(t, span, "money_flow.aggregate_id", id)
	wantAttr(t, span, "money_flow.outcome", "ok")
	if span.Status().Code != codes.Unset {
		t.Errorf("status = %v, want Unset", span.Status().Code)
	}
	if n := rec.histogramCount(t, "money_flow.eventstore.operation.duration",
		attribute.String("money_flow.operation", "append"), attribute.String("money_flow.outcome", "ok")); n != 1 {
		t.Errorf("append duration observations = %d, want 1", n)
	}
	if n := rec.sum(t, "money_flow.eventstore.events.appended",
		attribute.String("money_flow.event_type", "holder.v1.HolderEstablished")); n != 1 {
		t.Errorf("events appended = %d, want 1", n)
	}
}

// Losing an optimistic-concurrency race is the store answering correctly, not
// failing. It is recorded as a conflict, so contention on a stream is visible
// as a rate, without marking the trace as broken, and nothing is counted as
// appended.
func TestStore_ALostRaceIsAConflictNotAnError(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	inner := eventstore.NewMemoryStore()
	store := telemetry.NewStore(inner, rec.Providers)
	id := testutil.ID("contended")
	ctx := context.Background()

	if err := inner.Append(ctx, holderType, id, 0, &pb.HolderEstablished{Id: id}); err != nil {
		t.Fatalf("seed Append() error = %v", err)
	}
	err := store.Append(ctx, holderType, id, 0, &pb.HolderEstablished{Id: id})
	if !errors.Is(err, eventstore.ErrConcurrencyConflict) {
		t.Fatalf("Append() error = %v, want ErrConcurrencyConflict passed through", err)
	}

	span := rec.span(t, "eventstore.append")
	wantAttr(t, span, "money_flow.outcome", "conflict")
	if span.Status().Code != codes.Unset {
		t.Errorf("status = %v, want Unset: a lost race is not a fault", span.Status().Code)
	}
	if n := rec.histogramCount(t, "money_flow.eventstore.operation.duration",
		attribute.String("money_flow.outcome", "conflict")); n != 1 {
		t.Errorf("conflict observations = %d, want 1", n)
	}
	if n := rec.sum(t, "money_flow.eventstore.events.appended"); n != 0 {
		t.Errorf("events appended = %d, want 0 for an append that did not land", n)
	}
}

type failingStore struct{ eventstore.Store }

var errDown = errors.New("connection refused")

func (failingStore) Load(context.Context, string, string) ([]eventstore.Event, error) {
	return nil, errDown
}

// A store that cannot answer at all is a fault, and the span says so.
func TestStore_AnInfrastructureFailureIsAnError(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	store := telemetry.NewStore(failingStore{}, rec.Providers)

	if _, err := store.Load(context.Background(), holderType, testutil.ID("down")); !errors.Is(err, errDown) {
		t.Fatalf("Load() error = %v, want the inner error passed through", err)
	}

	span := rec.span(t, "eventstore.load")
	wantAttr(t, span, "money_flow.outcome", "error")
	if span.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", span.Status().Code)
	}
}

// The aggregate id is on the span, where it links a trace to a stream, and
// never on the metric, where every new aggregate would be a new time series.
func TestStore_AggregateIDNeverBecomesAMetricDimension(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	store := telemetry.NewStore(eventstore.NewMemoryStore(), rec.Providers)
	ctx := context.Background()

	for _, name := range []string{"a", "b", "c"} {
		if _, err := store.Load(ctx, holderType, testutil.ID(name)); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
	}

	h, ok := rec.metric(t, "money_flow.eventstore.operation.duration").Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatal("operation duration is not a float64 histogram")
	}
	if len(h.DataPoints) != 1 {
		t.Fatalf("got %d series for three loads of one aggregate type, want 1", len(h.DataPoints))
	}
	if _, found := h.DataPoints[0].Attributes.Value("money_flow.aggregate_id"); found {
		t.Error("duration carries money_flow.aggregate_id")
	}
}
