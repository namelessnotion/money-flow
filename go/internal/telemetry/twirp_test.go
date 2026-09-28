package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/twitchtv/twirp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/telemetry"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

const requestTransferSpan = "transfer.v1.TransferService/RequestTransfer"

// stubTransfers answers RequestTransfer with a fixed error, or accepts it.
type stubTransfers struct {
	pb.TransferService
	err error
}

func (s stubTransfers) RequestTransfer(_ context.Context, req *pb.RequestTransferRequest) (*pb.RequestTransferResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &pb.RequestTransferResponse{Id: req.GetId()}, nil
}

// serve puts svc behind the same HTTP handler and Twirp interceptor
// cmd/server installs, and returns its URL.
func serve(t *testing.T, rec *recording, svc pb.TransferService) string {
	t.Helper()
	twirpServer := pb.NewTransferServiceServer(svc, twirp.WithServerInterceptors(telemetry.Interceptor(rec.Providers)))
	mux := http.NewServeMux()
	mux.Handle(twirpServer.PathPrefix(), twirpServer)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(telemetry.Handler(rec.Providers, mux))
	t.Cleanup(srv.Close)
	return srv.URL
}

func requestTransfer(t *testing.T, url string, id string) error {
	t.Helper()
	client := pb.NewTransferServiceProtobufClient(url, http.DefaultClient)
	_, err := client.RequestTransfer(context.Background(), &pb.RequestTransferRequest{Id: id})
	return err
}

// One RPC is one span named for the command it carried, tagged with the
// aggregate it addressed. That id is what links the RPC to the saga work its
// events later trigger in the orchestrator.
func TestInterceptor_NamesTheSpanForTheCommandAndTheAggregate(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	url := serve(t, rec, stubTransfers{})
	id := testutil.ID("rpc")

	if err := requestTransfer(t, url, id); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	// One request is one server span, otelhttp's. This one sits inside it,
	// so a backend that counts server spans as requests counts each once.
	span := rec.span(t, requestTransferSpan)
	if span.SpanKind() != trace.SpanKindInternal {
		t.Errorf("span kind = %v, want internal", span.SpanKind())
	}
	var servers int
	for _, s := range rec.spans.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			servers++
		}
	}
	if servers != 1 {
		t.Errorf("got %d server spans for one request, want 1", servers)
	}
	wantAttr(t, span, "rpc.system", "twirp")
	wantAttr(t, span, "rpc.service", "transfer.v1.TransferService")
	wantAttr(t, span, "rpc.method", "RequestTransfer")
	wantAttr(t, span, "money_flow.aggregate_id", id)
	if n := rec.histogramCount(t, "rpc.server.call.duration",
		attribute.String("rpc.method", "RequestTransfer"),
		attribute.String("twirp.error_code", "ok")); n != 1 {
		t.Errorf("RequestTransfer duration observations = %d, want 1", n)
	}
}

// Twirp answers that are part of the contract (a retryable Aborted, a
// NotFound, a precondition the caller missed) are not server faults. Only a
// 5xx code marks the span as an error.
func TestInterceptor_OnlyServerFaultsAreErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err        twirp.Error
		wantCode   string
		wantStatus codes.Code
	}{
		{twirp.NewError(twirp.Aborted, "lost the race three times"), "aborted", codes.Unset},
		{twirp.NotFoundError("no such transfer"), "not_found", codes.Unset},
		{twirp.NewError(twirp.FailedPrecondition, "wallet closed"), "failed_precondition", codes.Unset},
		{twirp.InternalError("store unreachable"), "internal", codes.Error},
	} {
		t.Run(tc.wantCode, func(t *testing.T) {
			t.Parallel()
			rec := newRecording(t)
			url := serve(t, rec, stubTransfers{err: tc.err})

			if err := requestTransfer(t, url, testutil.ID(tc.wantCode)); err == nil {
				t.Fatal("RequestTransfer() error = nil, want the stub's error")
			}

			span := rec.span(t, requestTransferSpan)
			wantAttr(t, span, "twirp.error_code", tc.wantCode)
			if span.Status().Code != tc.wantStatus {
				t.Errorf("status = %v, want %v", span.Status().Code, tc.wantStatus)
			}
		})
	}
}

// A caller that sends W3C trace context continues its own trace here, which
// is all the Ruby backend needs to do to join one.
func TestHandler_ContinuesTheCallersTrace(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	url := serve(t, rec, stubTransfers{})

	callerCtx, caller := rec.Tracer.Tracer("caller").Start(context.Background(), "ruby")
	header := http.Header{}
	propagation.TraceContext{}.Inject(callerCtx, propagation.HeaderCarrier(header))
	ctx, err := twirp.WithHTTPRequestHeaders(context.Background(), header)
	if err != nil {
		t.Fatalf("WithHTTPRequestHeaders() error = %v", err)
	}
	client := pb.NewTransferServiceProtobufClient(url, http.DefaultClient)
	if _, err := client.RequestTransfer(ctx, &pb.RequestTransferRequest{Id: testutil.ID("joined")}); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	caller.End()

	httpSpan := rec.span(t, "POST /twirp/transfer.v1.TransferService/")
	rpcSpan := rec.span(t, requestTransferSpan)
	if got, want := httpSpan.Parent().SpanID(), caller.SpanContext().SpanID(); got != want {
		t.Errorf("HTTP span parent = %s, want the caller's %s", got, want)
	}
	if got, want := rpcSpan.Parent().SpanID(), httpSpan.SpanContext().SpanID(); got != want {
		t.Errorf("RPC span parent = %s, want the HTTP span %s", got, want)
	}
	if got, want := rpcSpan.SpanContext().TraceID(), caller.SpanContext().TraceID(); got != want {
		t.Errorf("trace = %s, want the caller's %s", got, want)
	}
}

// Health checks arrive every few seconds from something that is not a user.
// Tracing them would bury the traces anyone actually wants.
func TestHandler_DoesNotTraceHealthChecks(t *testing.T) {
	t.Parallel()
	rec := newRecording(t)
	url := serve(t, rec, stubTransfers{})

	resp, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	_ = resp.Body.Close()

	if ended := rec.spans.Ended(); len(ended) != 0 {
		t.Errorf("got %d spans for a health check, want 0", len(ended))
	}
}
