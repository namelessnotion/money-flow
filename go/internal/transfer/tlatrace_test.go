package transfer

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/tlatrace"
)

// errInjected is every fault faultyStore injects; the handlers see it as any
// other store error and answer twirp.Internal.
var errInjected = errors.New("injected fault")

// faultyStore injects the faults spec/eventstore.tla's Fail and lost-reply
// steps model, on Transfer streams only, where the spec models them. It sits
// above tlatrace.Store, so a trace records what the database did, not what
// the handler was told.
type faultyStore struct {
	eventstore.Store

	mu  sync.Mutex
	rng *rand.Rand
}

func (s *faultyStore) roll(p float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64() < p
}

func (s *faultyStore) pause() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Duration(s.rng.IntN(3000)) * time.Microsecond
}

func (s *faultyStore) Load(ctx context.Context, aggregateType, aggregateID string) ([]eventstore.Event, error) {
	if aggregateType == AggregateType && s.roll(0.03) {
		return nil, errInjected // fails before reading anything
	}
	return s.Store.Load(ctx, aggregateType, aggregateID)
}

func (s *faultyStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	if aggregateType != AggregateType {
		return s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	}
	// Widen the window between a Load and this Append, so racing Requests
	// really do interleave there.
	time.Sleep(s.pause())
	if s.roll(0.03) {
		return errInjected // fails before inserting anything
	}
	if err := s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...); err != nil {
		return err
	}
	if s.roll(0.05) {
		return errInjected // the INSERT landed, but the reply was lost
	}
	return nil
}

// TestTLATrace drives concurrent, same-id RequestTransfer and RequestReversal
// calls through real Twirp and the real Postgres store, with faults injected,
// and writes one trace per Transfer stream into TLA_TRACE_DIR for
// spec/trace/validate.sh to check against spec/eventstore.tla. It asserts
// nothing about the answers itself: whether they were allowed is TLC's call.
func TestTLATrace(t *testing.T) {
	dir := os.Getenv("TLA_TRACE_DIR")
	if dir == "" {
		t.Skip("set TLA_TRACE_DIR to record traces for spec/trace/validate.sh")
	}
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("TLA_TRACE_SEED"); s != "" {
		var err error
		if seed, err = strconv.ParseUint(s, 10, 64); err != nil {
			t.Fatalf("TLA_TRACE_SEED=%q: %v", s, err)
		}
	}
	t.Logf("TLA_TRACE_SEED=%d", seed)
	rng := rand.New(rand.NewPCG(seed, seed))
	ctx := context.Background()
	pg := eventstore.NewPostgresStore(testutil.Pool(t))
	lc := ledger.NewFakeClient()

	// The shared test database is append-only, so every run mints its own ids,
	// even when it replays a seed: the seed drives the workload, not the ids.
	run := time.Now().UnixNano()
	name := func(s string) string { return testutil.ID(fmt.Sprintf("tla-%d-%s", run, s)) }
	from, to := name("from"), name("to")
	openWallet(t, pg, from, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, pg, to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, pg, lc, from, name("token"), usd(1000))
	fundToken(t, lc, name("token"), 1000)

	rec := tlatrace.NewRecorder()
	store := &faultyStore{Store: tlatrace.NewStore(pg, rec), rng: rand.New(rand.NewPCG(seed, seed+1))}
	srv := httptest.NewServer(pb.NewTransferServiceServer(
		NewServer(store, lc, nil, nil), twirp.WithServerInterceptors(tlatrace.Interceptor(rec))))
	t.Cleanup(srv.Close)
	client := pb.NewTransferServiceProtobufClient(srv.URL, http.DefaultClient)

	const streams = 40
	for i := range streams {
		id := name(fmt.Sprintf("xfer-%d", i))
		calls := make([]func(), 2+rng.IntN(5))
		for j := range calls {
			switch amount := usd(400); {
			case rng.Float64() < 0.2: // a reversal of nothing, on the same id
				calls[j] = func() {
					_, _ = client.RequestReversal(ctx, &pb.RequestReversalRequest{Id: id, TransferId: name("no-such-transfer")})
				}
			default:
				if rng.Float64() < 0.4 {
					amount = usd(5000) // more than the wallet holds: rejected
				}
				calls[j] = func() { _, _ = client.RequestTransfer(ctx, transferRequest(id, from, to, amount, true)) }
			}
		}
		release := make(chan struct{})
		var wg sync.WaitGroup
		for _, call := range calls {
			wg.Go(func() {
				<-release
				call()
			})
		}
		close(release)
		wg.Wait()
	}

	outcomes := map[string]int{}
	for _, r := range rec.Records() {
		if r.Req == "" {
			t.Errorf("unattributed %s on %s%v: a writer the trace can't account for", r.Kind, r.Stream, r.Streams)
		}
		if r.Kind == tlatrace.KindAnswer || r.Kind == tlatrace.KindAppend {
			outcomes[r.Kind+" "+r.Outcome]++
		}
	}
	t.Logf("outcomes: %v", outcomes)

	paths, err := rec.WriteStreams(dir, tlatrace.Header{
		MaxAttempts: maxConcurrencyAttempts,
		Handlers: map[string][]string{
			"RequestTransfer": tlatrace.DecisionEvents(&pb.RequestTransferResponse{}),
			"RequestReversal": tlatrace.DecisionEvents(&pb.RequestReversalResponse{}),
		},
	})
	if err != nil {
		t.Fatalf("WriteStreams() error = %v", err)
	}
	t.Logf("wrote %d traces to %s", len(paths), dir)
}
