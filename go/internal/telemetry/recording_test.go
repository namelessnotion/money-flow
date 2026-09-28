package telemetry_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/namelessnotion/money_flow/go/internal/telemetry"
)

// recording is a set of Providers backed by in-memory exporters, so a test can
// assert on exactly the spans and metrics one decorator produced.
type recording struct {
	telemetry.Providers
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

func newRecording(t *testing.T) *recording {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	return &recording{
		Providers: telemetry.Providers{
			Tracer:     tp,
			Meter:      mp,
			Propagator: propagation.TraceContext{},
		},
		spans:  spans,
		reader: reader,
	}
}

// span returns the one ended span called name, failing if there is not
// exactly one.
func (r *recording) span(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range r.spans.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		var names []string
		for _, s := range r.spans.Ended() {
			names = append(names, s.Name())
		}
		t.Fatalf("got %d spans named %q, want 1; ended spans: %v", len(found), name, names)
	}
	return found[0]
}

func (r *recording) metric(t *testing.T, name string) metricdata.Metrics {
	t.Helper()
	m, ok := r.find(t, name)
	if !ok {
		t.Fatalf("no metric named %q was recorded", name)
	}
	return m
}

func (r *recording) find(t *testing.T, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// histogramCount is how many observations the histogram name recorded with
// every one of attrs among its attributes.
func (r *recording) histogramCount(t *testing.T, name string, attrs ...attribute.KeyValue) uint64 {
	t.Helper()
	h, ok := r.metric(t, name).Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q is not a float64 histogram", name)
	}
	var n uint64
	for _, dp := range h.DataPoints {
		if hasAll(dp.Attributes, attrs) {
			n += dp.Count
		}
	}
	return n
}

// sum is the total the counter name recorded with every one of attrs among
// its attributes; zero if it recorded nothing at all.
func (r *recording) sum(t *testing.T, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	m, found := r.find(t, name)
	if !found {
		return 0
	}
	s, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q is not an int64 sum", name)
	}
	var n int64
	for _, dp := range s.DataPoints {
		if hasAll(dp.Attributes, attrs) {
			n += dp.Value
		}
	}
	return n
}

func hasAll(set attribute.Set, want []attribute.KeyValue) bool {
	for _, kv := range want {
		if v, ok := set.Value(kv.Key); !ok || v != kv.Value {
			return false
		}
	}
	return true
}

func attr(s sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func wantAttr(t *testing.T, s sdktrace.ReadOnlySpan, key attribute.Key, want string) {
	t.Helper()
	got, ok := attr(s, key)
	if !ok {
		t.Errorf("span %q has no %s attribute, want %q", s.Name(), key, want)
		return
	}
	if got.String() != want {
		t.Errorf("span %q %s = %q, want %q", s.Name(), key, got.String(), want)
	}
}
