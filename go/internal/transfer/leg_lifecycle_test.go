package transfer

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/token"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

// commitCountingStore counts the commits a Transfer costs the event store —
// every successful Append or AppendAtomic is one Postgres transaction, and so
// one WAL flush, the resource that caps throughput — and which aggregate types
// those commits wrote to.
type commitCountingStore struct {
	eventstore.Store
	mu      sync.Mutex
	commits int
	written map[string]bool
}

func newCommitCountingStore(base eventstore.Store) *commitCountingStore {
	return &commitCountingStore{Store: base, written: map[string]bool{}}
}

func (s *commitCountingStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	err := s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	if err == nil {
		s.mu.Lock()
		s.commits++
		s.written[aggregateType] = true
		s.mu.Unlock()
	}
	return err
}

func (s *commitCountingStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	err := s.Store.AppendAtomic(ctx, writes...)
	if err == nil {
		s.mu.Lock()
		s.commits++
		for _, w := range writes {
			if len(w.Events) > 0 {
				s.written[w.AggregateType] = true
			}
		}
		s.mu.Unlock()
	}
	return err
}

func (s *commitCountingStore) snapshot() (commits int, written []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for aggregateType := range s.written {
		written = append(written, aggregateType)
	}
	slices.Sort(written)
	return s.commits, written
}

// legAggregateTypes is every aggregate type a Transfer writes to, sorted as
// commitCountingStore.snapshot reports them: its own stream, the Tokens it
// mints and moves, and the Wallet it mints into.
func legAggregateTypes() []string {
	types := []string{AggregateType, token.AggregateType, wallet.AggregateType}
	slices.Sort(types)
	return types
}

// fundedWallets opens a source Wallet holding one funded Token of 1000 and an
// empty destination Wallet, directly on base so none of it is counted.
func fundedWallets(t *testing.T, base eventstore.Store, lc ledger.Client) (from, to string) {
	t.Helper()
	from, to = testutil.ID("w1"), testutil.ID("w2")
	openWallet(t, base, from, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, base, to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, base, lc, from, testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)
	return from, to
}

// A Transfer's legs are entities inside it, not aggregates of their own
// (go/docs/adr/0009): an immediate Transfer's whole history is its own
// stream, plus the destination Token it mints and the balances it moves.
func TestImmediateTransfer_RecordsItsLegsOnItsOwnStreamOnly(t *testing.T) {
	t.Parallel()
	base := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	from, to := fundedWallets(t, base, lc)
	store := newCommitCountingStore(base)
	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()

	transferID := testutil.ID("xfer-immediate")
	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, from, to, usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	events := mustEvents(t, base, transferID)
	want := []string{
		eventstore.EventType(&pb.TransferRequestAccepted{}),
		eventstore.EventType(&pb.TransferPrepared{}),
		eventstore.EventType(&pb.TransferCommittingStarted{}),
		eventstore.EventType(&pb.TransferCommitted{}),
	}
	if got := eventTypes(events); !slices.Equal(got, want) {
		t.Errorf("transfer stream = %v, want %v", got, want)
	}

	// accept, prepare (mint + legs + the commit claim), then
	// TransferCommitted together with the balance of every Token it moved
	// (go/docs/adr/0010).
	commits, written := store.snapshot()
	if commits != 3 {
		t.Errorf("commits = %d, want 3", commits)
	}
	if !slices.Equal(written, legAggregateTypes()) {
		t.Errorf("wrote to aggregate types %v, want only %v", written, legAggregateTypes())
	}
}

func TestStagedTransfer_RecordsItsLegsOnItsOwnStreamOnly(t *testing.T) {
	t.Parallel()
	base := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	from, to := fundedWallets(t, base, lc)
	store := newCommitCountingStore(base)
	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()

	transferID := testutil.ID("xfer-staged")
	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, from, to, usd(400), true)); err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}
	if _, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: transferID}); err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if _, err := server.PostPendingTransfer(ctx, &pb.PostPendingTransferRequest{Id: transferID}); err != nil {
		t.Fatalf("PostPendingTransfer() error = %v", err)
	}

	want := []string{
		eventstore.EventType(&pb.TransferRequestAccepted{}),
		eventstore.EventType(&pb.TransferPrepared{}),
		eventstore.EventType(&pb.StagingTransferStarted{}),
		eventstore.EventType(&pb.TransferStaged{}),
		eventstore.EventType(&pb.TransferPending{}),
		eventstore.EventType(&pb.TransferCommittingStarted{}),
		eventstore.EventType(&pb.TransferCommitted{}),
	}
	if got := eventTypes(mustEvents(t, base, transferID)); !slices.Equal(got, want) {
		t.Errorf("transfer stream = %v, want %v", got, want)
	}
	// accept, prepare with the stage claim, TransferStaged with balances,
	// TransferPending, commit claim, TransferCommitted with balances.
	commits, written := store.snapshot()
	if commits != 6 {
		t.Errorf("commits = %d, want 6", commits)
	}
	if !slices.Equal(written, legAggregateTypes()) {
		t.Errorf("wrote to aggregate types %v, want only %v", written, legAggregateTypes())
	}
}

// The Transfer's own stream is where "no leg reaches two different outcomes"
// is enforced: appendSagaStep refuses any transition its lifecycle does not
// allow. This is the halt go/docs/adr/0005's stress test hit ("already Failed,
// cannot also become Staged"), now caught on the Transfer rather than on each
// leg.
func TestAppendSagaStep_RefusesATransitionTheLifecycleDoesNotAllow(t *testing.T) {
	t.Parallel()
	accepted := &pb.TransferRequestAccepted{Id: "x", Amount: usd(1)}
	prepared := &pb.TransferPrepared{Id: "x"}
	for name, tc := range map[string]struct {
		history []proto.Message
		next    proto.Message
	}{
		"failed cannot also become staged": {
			history: []proto.Message{accepted, prepared, &pb.TransferFailed{Id: "x"}},
			next:    &pb.TransferStaged{Id: "x"},
		},
		"committed cannot also become failed": {
			history: []proto.Message{accepted, prepared, &pb.TransferCommitted{Id: "x"}},
			next:    &pb.TransferFailed{Id: "x"},
		},
		"cancelled cannot also become committed": {
			history: []proto.Message{accepted, prepared, &pb.PreparedTransferCancelled{Id: "x"}},
			next:    &pb.TransferCommitted{Id: "x"},
		},
		"a prepared transfer is not cancelled as merely accepted": {
			history: []proto.Message{accepted, prepared},
			next:    &pb.AcceptedTransferCancelled{Id: "x"},
		},
		"a staged transfer does not commit without going pending": {
			history: []proto.Message{accepted, prepared, &pb.TransferStaged{Id: "x"}},
			next:    &pb.TransferCommitted{Id: "x"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := eventstore.NewMemoryStore()
			transferID := testutil.ID("xfer-" + name)
			ctx := context.Background()
			if err := store.Append(ctx, AggregateType, transferID, 0, tc.history...); err != nil {
				t.Fatalf("seed: %v", err)
			}
			server := NewServer(store, ledger.NewFakeClient(), nil, nil)

			err := server.appendSagaStep(ctx, transferID, tc.next)
			if err == nil || !strings.Contains(err.Error(), "cannot also become") {
				t.Fatalf("appendSagaStep() error = %v, want a refused transition", err)
			}
			if got := len(mustEvents(t, store, transferID)); got != len(tc.history) {
				t.Errorf("stream has %d events, want %d: a refused transition must record nothing", got, len(tc.history))
			}
		})
	}
}

// A retried saga step converges on the outcome it already recorded, even once
// a client's *Rejected response has landed on top of it.
func TestAppendSagaStep_ConvergesOnAnOutcomeBehindARejection(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	transferID := testutil.ID("xfer-converge")
	ctx := context.Background()
	if err := store.Append(ctx, AggregateType, transferID, 0,
		&pb.TransferRequestAccepted{Id: transferID, Amount: usd(1)},
		&pb.TransferPrepared{Id: transferID},
		&pb.TransferCommitted{Id: transferID},
		&pb.PostPendingTransferRejected{Id: transferID, Reason: "not pending"},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	server := NewServer(store, ledger.NewFakeClient(), nil, nil)

	if err := server.appendSagaStep(ctx, transferID, &pb.TransferCommitted{Id: transferID}); err != nil {
		t.Fatalf("appendSagaStep() error = %v, want convergence", err)
	}
	if got := len(mustEvents(t, store, transferID)); got != 4 {
		t.Errorf("stream has %d events, want 4: the outcome was already recorded", got)
	}
}

// With no per-leg record, a failed Transfer's own event carries why.
func TestCommit_TigerBeetleRejectionRecordsWhyOnTransferFailed(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	from, to := fundedWallets(t, store, lc)
	server := NewServer(store, &rejectingTransfersClient{Client: lc}, nil, nil)
	ctx := context.Background()

	transferID := testutil.ID("xfer-failed-reason")
	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, from, to, usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	events := mustEvents(t, store, transferID)
	msg, err := events[len(events)-1].Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	failed, ok := msg.(*pb.TransferFailed)
	if !ok {
		t.Fatalf("last event = %T, want TransferFailed", msg)
	}
	if !strings.Contains(failed.GetReason(), "tigerbeetle rejected commit") {
		t.Errorf("TransferFailed.reason = %q, want the ledger's refusal", failed.GetReason())
	}
}

func TestCancelAcceptedTransfer_RecordsTheReasonOnPreparedTransferCancelled(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	from, to := fundedWallets(t, store, lc)
	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()

	transferID := testutil.ID("xfer-cancel-reason")
	seedPreparedTransfer(t, ctx, server, store, transferID, from, to, usd(400))
	if _, err := server.CancelAcceptedTransfer(ctx, &pb.CancelAcceptedTransferRequest{Id: transferID, Reason: "customer changed their mind"}); err != nil {
		t.Fatalf("CancelAcceptedTransfer() error = %v", err)
	}

	events := mustEvents(t, store, transferID)
	msg, err := events[len(events)-1].Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	cancelled, ok := msg.(*pb.PreparedTransferCancelled)
	if !ok {
		t.Fatalf("last event = %T, want PreparedTransferCancelled", msg)
	}
	if cancelled.GetReason() != "customer changed their mind" {
		t.Errorf("PreparedTransferCancelled.reason = %q, want the caller's reason", cancelled.GetReason())
	}
}

// Cancelling a Prepared Transfer touches nothing outside the event store, so
// it is one append: no claim marker precedes it (go/docs/adr/0009). The
// append itself is the compare-and-swap that keeps it from racing a stage()
// or commit() claim.
func TestCancelAcceptedTransfer_CancelsAPreparedTransferInOneCommit(t *testing.T) {
	t.Parallel()
	base := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	from, to := fundedWallets(t, base, lc)
	ctx := context.Background()
	transferID := testutil.ID("xfer-cancel-one-commit")
	seedPreparedTransfer(t, ctx, NewServer(base, lc, nil, nil), base, transferID, from, to, usd(400))

	store := newCommitCountingStore(base)
	server := NewServer(store, lc, nil, nil)
	resp, err := server.CancelAcceptedTransfer(ctx, &pb.CancelAcceptedTransferRequest{Id: transferID, Reason: "client"})
	if err != nil {
		t.Fatalf("CancelAcceptedTransfer() error = %v", err)
	}
	if resp.GetAcceptedTransferCancelled() == nil {
		t.Fatalf("result = %v, want AcceptedTransferCancelled", resp.GetResult())
	}

	want := []string{
		eventstore.EventType(&pb.TransferRequestAccepted{}),
		eventstore.EventType(&pb.TransferPrepared{}),
		eventstore.EventType(&pb.PreparedTransferCancelled{}),
	}
	if got := eventTypes(mustEvents(t, base, transferID)); !slices.Equal(got, want) {
		t.Errorf("transfer stream = %v, want %v", got, want)
	}
	if commits, _ := store.snapshot(); commits != 1 {
		t.Errorf("commits = %d, want 1", commits)
	}
}

// A caller that loses the race to a step's outcome — a second
// CancelStagedTransfer, a Transaction rollback redelivered beside the first —
// reaches the step after the Transfer has already moved on. It must find
// nothing left to do: no claim on a finished Transfer, no second submission
// to TigerBeetle.
func TestSagaSteps_ClaimNothingOnceTheTransferHasMovedOn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stage  bool
		finish func(ctx context.Context, s *Server, transferID string) error
		again  func(ctx context.Context, s *Server, transferID string) error
	}{
		"cancelStaged after the cancel landed": {
			stage: true,
			finish: func(ctx context.Context, s *Server, transferID string) error {
				_, err := s.CancelStagedTransfer(ctx, &pb.CancelStagedTransferRequest{Id: transferID, Reason: "first"})
				return err
			},
			again: func(ctx context.Context, s *Server, transferID string) error {
				return s.cancelStaged(ctx, transferID, "second")
			},
		},
		"commit after the commit landed": {
			stage:  false,
			finish: func(context.Context, *Server, string) error { return nil },
			again: func(ctx context.Context, s *Server, transferID string) error {
				return s.commit(ctx, transferID)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := eventstore.NewMemoryStore()
			fake := ledger.NewFakeClient()
			from, to := fundedWallets(t, store, fake)
			lc := &createTransfersCountingClient{Client: fake}
			server := NewServer(store, lc, nil, nil)
			ctx := context.Background()
			transferID := testutil.ID("xfer-moved-on")
			if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, from, to, usd(400), tc.stage)); err != nil {
				t.Fatalf("RequestTransfer() error = %v", err)
			}
			if err := tc.finish(ctx, server, transferID); err != nil {
				t.Fatalf("finish: %v", err)
			}
			before := len(mustEvents(t, store, transferID))
			submitted := lc.count()

			if err := tc.again(ctx, server, transferID); err != nil {
				t.Fatalf("second call error = %v, want nil", err)
			}
			if got := len(mustEvents(t, store, transferID)); got != before {
				t.Errorf("stream grew from %d to %d events: a finished Transfer was claimed again", before, got)
			}
			if got := lc.count(); got != submitted {
				t.Errorf("ledger.CreateTransfers called %d more times, want 0", got-submitted)
			}
		})
	}
}

// balanceRacingStore lands a concurrent TokenBalanceRecorded on raceTokenID
// just before the first atomic write carrying a TransferCommitted — another
// Transfer's step recording the same, shared source Token — so that write
// loses on the Token's stream.
type balanceRacingStore struct {
	eventstore.Store
	raceTokenID string
	once        sync.Once
	raced       bool
}

func (s *balanceRacingStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	for _, w := range writes {
		if w.AggregateType != AggregateType || len(w.Events) == 0 || eventstore.EventType(w.Events[0]) != eventstore.EventType(&pb.TransferCommitted{}) {
			continue
		}
		s.once.Do(func() {
			events, err := s.Load(ctx, token.AggregateType, s.raceTokenID)
			if err != nil {
				return
			}
			s.raced = s.Append(ctx, token.AggregateType, s.raceTokenID, int64(len(events)),
				&tokenpb.TokenBalanceRecorded{Id: s.raceTokenID, Currency: "USD", PostedMinorUnits: 1}) == nil
		})
	}
	return s.Store.AppendAtomic(ctx, writes...)
}

// A step's outcome and the balances it moved land together, so losing the
// race on a shared Token's stream retries both: the outcome is recorded once,
// and the Token's last balance is still the ledger's, read after the balance
// that beat it.
func TestCommit_RetriesTheOutcomeWithItsBalancesWhenAShareTokenMoves(t *testing.T) {
	t.Parallel()
	base := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	from, to := fundedWallets(t, base, lc)
	store := &balanceRacingStore{Store: base, raceTokenID: testutil.ID("t1")}
	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()

	transferID := testutil.ID("xfer-balance-race")
	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, from, to, usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if !store.raced {
		t.Fatal("the concurrent balance never landed; the test proved nothing")
	}

	committed := 0
	for _, e := range mustEvents(t, base, transferID) {
		if e.EventType == eventstore.EventType(&pb.TransferCommitted{}) {
			committed++
		}
	}
	if committed != 1 {
		t.Errorf("TransferCommitted recorded %d times, want 1", committed)
	}
	assertBalanceRecorded(t, base, testutil.ID("t1"), 600, 0, 0)
}

// prepare() claims the step that follows it in the same write as
// TransferPrepared (go/docs/adr/0010), so a driver that dies between the two
// leaves a claim behind, not an unclaimed Prepared Transfer. That claim is
// abandoned like any other: once stale, the same step takes it over and the
// Transfer carries on.
func TestPrepare_ClaimsTheNextStepSoACrashedDispatchIsTakenOver(t *testing.T) {
	// Deliberately not t.Parallel(): mutates claimStaleAfter.
	defer swapClaimStaleAfter(20 * time.Millisecond)()

	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	from, to := fundedWallets(t, store, lc)
	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	transferID := testutil.ID("xfer-crashed-after-prepare")
	if _, err := server.RequestTransfer(ctx, transferRequest(transferID, from, to, usd(400), true)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	// The driver prepares, then dies before staging.
	if _, err := server.prepare(ctx, transferID); err != nil {
		t.Fatalf("prepare() error = %v", err)
	}
	want := []string{
		eventstore.EventType(&pb.TransferRequestAccepted{}),
		eventstore.EventType(&pb.TransferPrepared{}),
		eventstore.EventType(&pb.StagingTransferStarted{}),
	}
	if got := eventTypes(mustEvents(t, store, transferID)); !slices.Equal(got, want) {
		t.Fatalf("after prepare() the stream is %v, want %v", got, want)
	}

	time.Sleep(2 * claimStaleAfter)
	if err := server.Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if state := currentStateOf(t, store, transferID); state != stateStaged {
		t.Errorf("state = %v, want staged: the abandoned claim should have been taken over", state)
	}
}
