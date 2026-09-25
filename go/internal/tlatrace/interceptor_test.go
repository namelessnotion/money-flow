package tlatrace_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/twitchtv/twirp"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/tlatrace"
)

// stubTransfers answers RequestTransfer with a fixed response or error, and
// remembers which traced Request each call's context carried.
type stubTransfers struct {
	pb.TransferService
	resp *pb.RequestTransferResponse
	err  error

	mu   sync.Mutex
	seen []string
}

func (s *stubTransfers) RequestTransfer(ctx context.Context, _ *pb.RequestTransferRequest) (*pb.RequestTransferResponse, error) {
	req, _ := tlatrace.RequestFrom(ctx)
	s.mu.Lock()
	s.seen = append(s.seen, req)
	s.mu.Unlock()
	return s.resp, s.err
}

// serve puts svc behind real Twirp with the tracing interceptor installed,
// and returns a client for it.
func serve(t *testing.T, svc pb.TransferService, rec *tlatrace.Recorder) pb.TransferService {
	t.Helper()
	srv := httptest.NewServer(pb.NewTransferServiceServer(svc, twirp.WithServerInterceptors(tlatrace.Interceptor(rec))))
	t.Cleanup(srv.Close)
	return pb.NewTransferServiceProtobufClient(srv.URL, http.DefaultClient)
}

func accepted(id string) *pb.RequestTransferResponse {
	return &pb.RequestTransferResponse{Id: id, Result: &pb.RequestTransferResponse_TransferRequestAccepted{
		TransferRequestAccepted: &pb.TransferRequestAccepted{Id: id},
	}}
}

// A call is recorded as the command it asked for on its stream, then the
// decision it was answered with, and the handler's context carries the same
// Request id so the store calls in between are attributed to it.
func TestInterceptor_RecordsTheCommandAndTheDecisionItWasAnsweredWith(t *testing.T) {
	t.Parallel()
	id := testutil.ID("decided")
	rec := tlatrace.NewRecorder()
	stub := &stubTransfers{resp: accepted(id)}

	if _, err := serve(t, stub, rec).RequestTransfer(context.Background(), &pb.RequestTransferRequest{Id: id}); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	records := rec.Records()
	if len(records) != 2 {
		t.Fatalf("Records() = %+v, want a Receive and an Answer", records)
	}
	receive, answer := records[0], records[1]
	if receive.Kind != tlatrace.KindReceive || receive.Command != "RequestTransfer" || receive.Stream != id {
		t.Errorf("first record = %+v, want Receive of RequestTransfer on %s", receive, id)
	}
	want := eventstore.EventType(&pb.TransferRequestAccepted{})
	if answer.Kind != tlatrace.KindAnswer || answer.Outcome != want || answer.Stream != id {
		t.Errorf("second record = %+v, want Answer %s on %s", answer, want, id)
	}
	if answer.Req != receive.Req || receive.Req == "" {
		t.Errorf("Receive by %q, Answer to %q: want the same, non-empty Request", receive.Req, answer.Req)
	}
	if len(stub.seen) != 1 || stub.seen[0] != receive.Req {
		t.Errorf("handler context carried %v, want [%s]", stub.seen, receive.Req)
	}
}

// Callers can't tell anything from an error about whether their command was
// decided; the trace keeps Aborted (lost every race) apart from everything
// else (Failed), as the spec does.
func TestInterceptor_RecordsErrorsAsAbortedOrFailed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"aborted":  {twirp.NewError(twirp.Aborted, "could not converge"), tlatrace.OutcomeAborted},
		"internal": {twirp.InternalError("boom"), tlatrace.OutcomeFailed},
		"invalid":  {twirp.InvalidArgumentError("id", "is not a UUID"), tlatrace.OutcomeFailed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := tlatrace.NewRecorder()

			_, _ = serve(t, &stubTransfers{err: tc.err}, rec).RequestTransfer(
				context.Background(), &pb.RequestTransferRequest{Id: testutil.ID(name)})

			records := rec.Records()
			if len(records) != 2 || records[1].Outcome != tc.want {
				t.Errorf("Records() = %+v, want an Answer of %q", records, tc.want)
			}
		})
	}
}

// A response with nothing in its result oneof decided nothing, which no step
// of the spec allows; the trace says so rather than guessing.
func TestInterceptor_RecordsAResponseWithoutADecisionAsUndecided(t *testing.T) {
	t.Parallel()
	rec := tlatrace.NewRecorder()
	id := testutil.ID("undecided")

	if _, err := serve(t, &stubTransfers{resp: &pb.RequestTransferResponse{Id: id}}, rec).RequestTransfer(
		context.Background(), &pb.RequestTransferRequest{Id: id}); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	if records := rec.Records(); len(records) != 2 || records[1].Outcome != tlatrace.OutcomeUndecided {
		t.Errorf("Records() = %+v, want an Answer of %q", records, tlatrace.OutcomeUndecided)
	}
}

// Concurrent calls are distinct Requests, even for the same command on the
// same stream: that's the race the spec is about.
func TestInterceptor_GivesEveryCallItsOwnRequest(t *testing.T) {
	t.Parallel()
	id := testutil.ID("contended")
	rec := tlatrace.NewRecorder()
	client := serve(t, &stubTransfers{resp: accepted(id)}, rec)

	const calls = 20
	var wg sync.WaitGroup
	for range calls {
		wg.Go(func() {
			_, _ = client.RequestTransfer(context.Background(), &pb.RequestTransferRequest{Id: id})
		})
	}
	wg.Wait()

	reqs := map[string]bool{}
	for _, r := range rec.Records() {
		if r.Kind == tlatrace.KindReceive {
			reqs[r.Req] = true
		}
	}
	if len(reqs) != calls {
		t.Errorf("%d distinct Requests received, want %d", len(reqs), calls)
	}
}
