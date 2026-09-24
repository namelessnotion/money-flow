package transfer

import (
	"context"
	"errors"
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
)

// swapClaimStaleAfter shrinks the package-level claimStaleAfter for the
// duration of a test (claimForDispatch, go/docs/adr/0005) so a test that
// needs a marker to actually go stale doesn't have to sleep for the real
// 5s production value. Returns a restore func; callers should defer it.
func swapClaimStaleAfter(d time.Duration) func() {
	orig := claimStaleAfter
	claimStaleAfter = d
	return func() { claimStaleAfter = orig }
}

// barrierStore holds Append/AppendAtomic calls to one named stream open
// until enough concurrent callers have arrived at once, so a race is
// certain rather than merely likely — the same technique go/internal/saga's
// helpers_test.go uses for TestOrchestrator_ConcurrentTriggersOnOneTransactionConverge,
// ported here because that helper lives in a different package's _test.go
// file. See go/docs/adr/0005.
type barrierStore struct {
	eventstore.Store

	aggregateType, aggregateID string
	need                       int

	mu       sync.Mutex
	waiting  int
	released bool
	gate     chan struct{}

	conflicts int
}

func newBarrierStore(base eventstore.Store, aggregateType, aggregateID string, need int) *barrierStore {
	return &barrierStore{Store: base, aggregateType: aggregateType, aggregateID: aggregateID, need: need, gate: make(chan struct{})}
}

func (s *barrierStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	s.hold(aggregateType, aggregateID)
	err := s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	if errors.Is(err, eventstore.ErrConcurrencyConflict) {
		s.mu.Lock()
		s.conflicts++
		s.mu.Unlock()
	}
	return err
}

func (s *barrierStore) hold(aggregateType, aggregateID string) {
	if aggregateType != s.aggregateType || aggregateID != s.aggregateID {
		return
	}
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.waiting++
	if s.waiting >= s.need {
		s.released = true
		close(s.gate)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	select {
	case <-s.gate:
	case <-time.After(2 * time.Second):
	}
}

func (s *barrierStore) tripped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released
}

func (s *barrierStore) sawConflict() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conflicts > 0
}

// createTransfersCountingClient wraps ledger.Client to count CreateTransfers calls, so a
// concurrency test can assert a side effect happened exactly once rather
// than inferring it from the event log alone.
type createTransfersCountingClient struct {
	ledger.Client
	mu    sync.Mutex
	calls int
}

func (c *createTransfersCountingClient) CreateTransfers(ctx context.Context, transfers []ledger.Transfer) ([]ledger.TransferResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.Client.CreateTransfers(ctx, transfers)
}

func (c *createTransfersCountingClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// seedPreparedTransfer hand-seeds a minimal TransferRequestAccepted (only
// the fields prepare() itself reads) and runs the real prepare() once,
// single-threaded, landing the Transfer at exactly statePrepared — the one
// state no synchronous public-API call stops at (RequestTransfer always
// drives runSaga through to Staged or Committed in one call), which a
// concurrency test on stage() needs as its starting point.
func seedPreparedTransfer(t *testing.T, ctx context.Context, s *Server, store eventstore.Store, transferID, fromWallet, toWallet string, amount *sharedpb.Money) {
	t.Helper()
	if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
		Id: transferID, FromWalletId: fromWallet, ToWalletId: toWallet, Amount: amount, Stage: true,
	}); err != nil {
		t.Fatalf("seed TransferRequestAccepted: %v", err)
	}
	if err := s.prepare(ctx, transferID); err != nil {
		t.Fatalf("prepare(): %v", err)
	}
	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != statePrepared {
		t.Fatalf("state after seed+prepare = %v, want prepared", currentState(events))
	}
}

// TestStage_ConcurrentDispatchConvergesWithoutDoubleSubmission reproduces
// the go/docs/adr/0005 bug directly: the synchronous RPC path and the async
// orchestrator's Resume both fold the same statePrepared read and, before
// this fix, both entered stage() and both submitted to TigerBeetle. Without
// the claim, this test's barrier makes that race certain instead of
// occasional, and the second submitBatch call would legitimately be
// rejected as a duplicate reservation, driving one goroutine into
// compensate() (TransferFailed) while the other's already-in-flight stage()
// goes on to record TransferStaged — exactly the "already Failed, cannot
// also become Staged" halt go/docs/adr/0005's stress test hit, which
// appendSagaStep's transitions guard now refuses on the Transfer itself.
func TestStage_ConcurrentDispatchConvergesWithoutDoubleSubmission(t *testing.T) {
	t.Parallel()
	base := eventstore.NewMemoryStore()
	fake := ledger.NewFakeClient()
	lc := &createTransfersCountingClient{Client: fake}
	transferID := testutil.ID("xfer-race")
	w1, w2, t1 := testutil.ID("w1"), testutil.ID("w2"), testutil.ID("t1")

	openWallet(t, base, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, base, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, base, lc, w1, t1, usd(1000))
	fundToken(t, lc, t1, 1000)

	server := NewServer(base, lc, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, base, transferID, w1, w2, usd(400))
	lc.mu.Lock()
	lc.calls = 0 // only count calls made by the race below, not mint/fund setup
	lc.mu.Unlock()

	store := newBarrierStore(base, AggregateType, transferID, 2)
	racingServer := NewServer(store, lc, nil, nil)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = racingServer.Resume(ctx, transferID)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Resume() %d error = %v (the claim should make the loser a no-op, never a contradiction)", i, err)
		}
	}
	if !store.tripped() {
		t.Fatal("the two goroutines never raced on the same append; the test proved nothing")
	}
	if !store.sawConflict() {
		t.Error("no ErrConcurrencyConflict occurred; the claim's CAS was never actually contested")
	}
	if got := lc.count(); got != 1 {
		t.Errorf("ledger.CreateTransfers called %d times, want exactly 1 (the claim must stop the loser before it submits)", got)
	}

	events, err := base.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	switch state := currentState(events); state {
	case stateStaged, stateFailed:
		// Both are legitimate outcomes of one real TigerBeetle submission;
		// what matters is that exactly one happened and both callers agree.
	default:
		t.Errorf("final state = %v, want staged or failed", state)
	}
}

// TestCommit_ConcurrentPostPendingTransferConvergesWithoutDoubleSubmission
// covers the same class of bug one level down the state machine, reachable
// entirely through the public API: two concurrent PostPendingTransfer calls
// for the same already-staged-and-confirmed Transfer (e.g. a client retry
// racing itself) must not both submit the posting batch to TigerBeetle.
func TestCommit_ConcurrentPostPendingTransferConvergesWithoutDoubleSubmission(t *testing.T) {
	t.Parallel()
	base := eventstore.NewMemoryStore()
	fake := ledger.NewFakeClient()
	lc := &createTransfersCountingClient{Client: fake}
	transferID := testutil.ID("xfer-race-commit")
	w1, w2, t1 := testutil.ID("w1"), testutil.ID("w2"), testutil.ID("t1")

	openWallet(t, base, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, base, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, base, lc, w1, t1, usd(1000))
	fundToken(t, lc, t1, 1000)

	server := NewServer(base, lc, nil, nil)
	ctx := context.Background()
	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, w1, w2, usd(400), true)); err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}
	if _, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: transferID}); err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	lc.mu.Lock()
	lc.calls = 0 // only count calls made by the race below
	lc.mu.Unlock()

	store := newBarrierStore(base, AggregateType, transferID, 2)
	racingServer := NewServer(store, lc, nil, nil)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := racingServer.PostPendingTransfer(ctx, &pb.PostPendingTransferRequest{Id: transferID})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent PostPendingTransfer() %d error = %v", i, err)
		}
	}
	if !store.tripped() {
		t.Fatal("the two goroutines never raced on the same append; the test proved nothing")
	}
	if got := lc.count(); got != 1 {
		t.Errorf("ledger.CreateTransfers called %d times, want exactly 1", got)
	}

	events, err := base.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if state := currentState(events); state != stateCommitted && state != stateFailed {
		t.Errorf("final state = %v, want committed or failed", state)
	}
}

// TestStage_OrphanedClaimDoesNotBlockRetry guards the crash-recovery
// property go/docs/adr/0005 depends on: a claim marker left behind by a
// caller that crashed before finishing must not permanently stall the
// Transfer. currentState() ignores the marker, so a later call re-enters
// stage() exactly as if nothing had claimed it, takes a fresh claim, and
// completes normally.
func TestStage_OrphanedClaimDoesNotBlockRetry(t *testing.T) {
	// Deliberately not t.Parallel(): it mutates the package-level
	// claimStaleAfter, which every other test in this package reads inside
	// claimForDispatch. Non-parallel tests run to completion in isolation
	// before any t.Parallel() test's body starts, so this is race-free only
	// as long as it stays sequential.
	defer swapClaimStaleAfter(20 * time.Millisecond)()

	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	transferID := testutil.ID("xfer-orphan")
	w1, w2, t1 := testutil.ID("w1"), testutil.ID("w2"), testutil.ID("t1")

	openWallet(t, store, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, w1, t1, usd(1000))
	fundToken(t, lc, t1, 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, store, transferID, w1, w2, usd(400))

	// Simulate a crash: a claim landed, but nothing after it did (no
	// TigerBeetle submission, no terminal append).
	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := store.Append(ctx, AggregateType, transferID, int64(len(events)), &pb.StagingTransferStarted{Id: transferID}); err != nil {
		t.Fatalf("seed orphaned claim: %v", err)
	}

	events, err = store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if currentState(events) != statePrepared {
		t.Fatalf("state after orphaned claim = %v, want still prepared (the marker must not change the fold)", currentState(events))
	}

	time.Sleep(2 * claimStaleAfter) // let the seeded marker actually go stale
	if err := server.Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume() after an orphaned claim: error = %v, want a clean retry", err)
	}

	events, err = store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if state := currentState(events); state != stateStaged {
		t.Fatalf("state = %v, want staged: an orphaned claim must not permanently stall the Transfer", state)
	}
}

// TestStage_DoesNotRaceAFreshInFlightClaimFromADifferentTransition
// reproduces the gap the first version of go/docs/adr/0005's fix missed:
// currentState() ignores every claim marker, so a caller that reloads the
// stream *after* a marker has landed (but before its winner has finished)
// used to see the same pre-claim coarse state and win an independent claim
// on the *next* sequence slot — racing the still-in-flight first one
// against the same legs. Concretely: CancelAcceptedTransfer wins
// CancellingPreparedTransferStarted; every event on a Transfer's own stream
// — including that marker itself — is published to transfer-events
// (go/docs/adr/0001), so the orchestrator's Resume fires on it and used to
// win a fresh StagingTransferStarted claim and reserve every leg in
// TigerBeetle while the cancel was still running: the exact "already
// Cancelled, cannot also become Staged" contradiction this decision exists
// to prevent.
//
// This test seeds that in-flight marker directly (standing in for "the
// cancel RPC's claim landed but its appendSagaStep hasn't run yet") and
// calls stage() concurrently — simulating the re-entrant Resume —
// asserting it blocks in claimForDispatch rather than taking its own claim,
// never touches TigerBeetle, and converges to whatever the in-flight
// caller's real resolution turns out to be once that lands.
func TestStage_DoesNotRaceAFreshInFlightClaimFromADifferentTransition(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	fake := ledger.NewFakeClient()
	lc := &createTransfersCountingClient{Client: fake}
	transferID := testutil.ID("xfer-in-flight-cancel")
	w1, w2, t1 := testutil.ID("w1"), testutil.ID("w2"), testutil.ID("t1")

	openWallet(t, store, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, w1, t1, usd(1000))
	fundToken(t, lc, t1, 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, store, transferID, w1, w2, usd(400))
	lc.mu.Lock()
	lc.calls = 0
	lc.mu.Unlock()

	// Stand in for "CancelAcceptedTransfer's claim landed": append the
	// marker directly, without running the rest of cancelPrepared() yet, so
	// it's genuinely still in flight from a fresh reader's point of view.
	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := store.Append(ctx, AggregateType, transferID, int64(len(events)),
		&pb.CancellingPreparedTransferStarted{Id: transferID, Reason: "test"},
	); err != nil {
		t.Fatalf("seed in-flight cancel claim: %v", err)
	}

	// The re-entrant Resume the marker's own Kafka publication would
	// trigger, racing the still-in-flight cancel.
	stageDone := make(chan error, 1)
	go func() {
		stageDone <- server.stage(ctx, transferID)
	}()

	// Give stage() time to reach claimForDispatch's poll loop, then confirm
	// it is genuinely waiting rather than having already proceeded.
	time.Sleep(10 * claimPollInterval)
	select {
	case err := <-stageDone:
		t.Fatalf("stage() returned (err=%v) before the in-flight cancel resolved; it should still be waiting", err)
	default:
	}
	if got := lc.count(); got != 0 {
		t.Fatalf("ledger.CreateTransfers called %d times while a different transition was still claimed, want 0", got)
	}

	// Now let the cancel actually finish, the way cancelPrepared() itself
	// would: append the terminal event.
	if err := server.appendSagaStep(ctx, transferID, &pb.PreparedTransferCancelled{Id: transferID, Reason: "test"}); err != nil {
		t.Fatalf("appendSagaStep(PreparedTransferCancelled) error = %v", err)
	}

	select {
	case err := <-stageDone:
		if err != nil {
			t.Fatalf("stage() error = %v, want nil once the in-flight cancel resolved", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stage() never returned after the in-flight cancel resolved")
	}
	if got := lc.count(); got != 0 {
		t.Errorf("ledger.CreateTransfers called %d times, want 0: stage() must never have submitted anything", got)
	}

	events, err = store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if state := currentState(events); state != stateCancelled {
		t.Errorf("final state = %v, want cancelled", state)
	}
}

// TestStage_SeesALiveClaimBehindAConfirmStagedTransferRejection reproduces
// the second gap found in go/docs/adr/0005's design: claimForDispatch used
// to check only the stream's literal trailing event for a live marker.
// ConfirmStagedTransfer, CancelStagedTransfer, and PostPendingTransfer each
// record their own *Rejected response onto the Transfer's own stream when
// called against a state that doesn't match what they expected — none of
// which advances currentState()'s fold — so one of those landing on top of
// a still-live marker hid it from a fresh reader. Concretely: while
// stage() holds the claim and the Transfer is still Prepared, a client
// calls ConfirmStagedTransfer; its rejection lands on the stream; the
// orchestrator's Resume (triggered by either event) then sees no marker at
// the literal tail and stages the Transfer a second time.
func TestStage_SeesALiveClaimBehindAConfirmStagedTransferRejection(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	fake := ledger.NewFakeClient()
	lc := &createTransfersCountingClient{Client: fake}
	transferID := testutil.ID("xfer-rejection-hides-marker")
	w1, w2, t1 := testutil.ID("w1"), testutil.ID("w2"), testutil.ID("t1")

	openWallet(t, store, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, w1, t1, usd(1000))
	fundToken(t, lc, t1, 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, store, transferID, w1, w2, usd(400))
	lc.mu.Lock()
	lc.calls = 0
	lc.mu.Unlock()

	// Stand in for "stage() already claimed this and is still working":
	// append the marker directly, without running the rest of stage() yet.
	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := store.Append(ctx, AggregateType, transferID, int64(len(events)),
		&pb.StagingTransferStarted{Id: transferID},
	); err != nil {
		t.Fatalf("seed in-flight stage claim: %v", err)
	}

	// The client calls ConfirmStagedTransfer while the Transfer is still
	// Prepared (stage() hasn't appended TransferStaged yet): its handler
	// records a rejection onto the Transfer's own stream, landing right on
	// top of the still-live marker.
	confirmResp, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: transferID})
	if err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if confirmResp.GetConfirmStagedTransferRejected() == nil {
		t.Fatalf("result = %v, want ConfirmStagedTransferRejected (the Transfer is still Prepared, not Staged)", confirmResp.GetResult())
	}
	events, err = store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if last := events[len(events)-1]; last.EventType != eventstore.EventType(&pb.ConfirmStagedTransferRejected{}) {
		t.Fatalf("trailing event = %q, want the rejection to have landed on top of the marker", last.EventType)
	}

	// The re-entrant Resume the marker's own Kafka publication would
	// trigger, racing the still-in-flight stage() — now with the rejection
	// sitting on top of the marker.
	stageDone := make(chan error, 1)
	go func() {
		stageDone <- server.stage(ctx, transferID)
	}()

	time.Sleep(10 * claimPollInterval)
	select {
	case err := <-stageDone:
		t.Fatalf("stage() returned (err=%v) before the in-flight claim resolved; the rejection must have hidden the live marker underneath it", err)
	default:
	}
	if got := lc.count(); got != 0 {
		t.Fatalf("ledger.CreateTransfers called %d times while the original claim was still live, want 0", got)
	}

	// Let the original stage() finish, the way stage() itself would once its
	// reservations landed.
	if err := server.appendSagaStep(ctx, transferID, &pb.TransferStaged{Id: transferID}); err != nil {
		t.Fatalf("appendSagaStep(TransferStaged) error = %v", err)
	}

	select {
	case err := <-stageDone:
		if err != nil {
			t.Fatalf("stage() error = %v, want nil once the in-flight claim resolved", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stage() never returned after the in-flight claim resolved")
	}
	if got := lc.count(); got != 0 {
		t.Errorf("ledger.CreateTransfers called %d times, want 0: the re-entrant stage() must never have submitted anything", got)
	}
}

// TestRequireClaim_DetectsSupersessionAfterAStaleReclaim covers the third
// gap: claimStaleAfter alone only decides who is *allowed* to reclaim — the
// original claimant never checked whether it still held the claim before
// its own side effects, so a merely slow (not crashed) winner could be
// reclaimed out from under itself and both callers would proceed. This
// checks requireClaim/stillHoldsClaim directly: once a second caller wins
// a stale reclaim, the first caller's original claimedSeq must no longer
// read as held.
func TestRequireClaim_DetectsSupersessionAfterAStaleReclaim(t *testing.T) {
	// Deliberately not t.Parallel(): mutates the package-level
	// claimStaleAfter (see TestStage_OrphanedClaimDoesNotBlockRetry).
	defer swapClaimStaleAfter(20 * time.Millisecond)()

	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	transferID := testutil.ID("xfer-stale-reclaim")
	w1, w2, t1 := testutil.ID("w1"), testutil.ID("w2"), testutil.ID("t1")

	openWallet(t, store, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, w1, t1, usd(1000))
	fundToken(t, lc, t1, 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, store, transferID, w1, w2, usd(400))

	claimedSeqA, wonA, err := server.claimForDispatch(ctx, transferID, statePrepared, &pb.StagingTransferStarted{Id: transferID})
	if err != nil || !wonA {
		t.Fatalf("A's claimForDispatch() = (%d, %v, %v), want (>0, true, nil)", claimedSeqA, wonA, err)
	}

	// A is legitimately slow, not crashed — but old enough that the marker
	// looks abandoned to anyone else reading it.
	time.Sleep(2 * claimStaleAfter)

	claimedSeqB, wonB, err := server.claimForDispatch(ctx, transferID, statePrepared, &pb.StagingTransferStarted{Id: transferID})
	if err != nil || !wonB {
		t.Fatalf("B's claimForDispatch() = (%d, %v, %v), want (>0, true, nil)", claimedSeqB, wonB, err)
	}
	if claimedSeqB == claimedSeqA {
		t.Fatalf("B claimed the same sequence as A (%d); the test didn't provoke a reclaim", claimedSeqA)
	}

	if held, err := server.stillHoldsClaim(ctx, transferID, claimedSeqA); err != nil {
		t.Fatalf("stillHoldsClaim(A) error = %v", err)
	} else if held {
		t.Error("A still reads as holding the claim after B reclaimed it")
	}
	if err := server.requireClaim(ctx, transferID, claimedSeqA); !errors.Is(err, errClaimSuperseded) {
		t.Errorf("requireClaim(A) error = %v, want errClaimSuperseded", err)
	}

	if held, err := server.stillHoldsClaim(ctx, transferID, claimedSeqB); err != nil {
		t.Fatalf("stillHoldsClaim(B) error = %v", err)
	} else if !held {
		t.Error("B does not read as holding the claim it just won")
	}
}

// skewedClockStore wraps eventstore.Store but answers Now() with a
// deliberately offset clock, standing in for the kind of Postgres-vs-Go-host
// drift go/docs/adr/0005 describes: MemoryStore stamps Event.OccurredAt
// using the real clock, so skewing only what Now() reports reproduces the
// mismatch without needing an actual second clock in the test.
type skewedClockStore struct {
	eventstore.Store
	skew time.Duration
}

func (s *skewedClockStore) Now(ctx context.Context) (time.Time, error) {
	now, err := s.Store.Now(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(s.skew), nil
}

// TestClaimForDispatch_UsesTheStoresClockNotTheHostClock proves
// claimForDispatch's staleness check is measured on eventstore.Store.Now(),
// not time.Now(): a store whose clock reads an hour behind the one that
// actually stamped the marker must keep treating it as fresh long after
// real wall-clock time has passed claimStaleAfter, because (skewed now) -
// occurred_at stays far short of claimStaleAfter regardless of how much
// real time elapses in the meantime.
func TestClaimForDispatch_UsesTheStoresClockNotTheHostClock(t *testing.T) {
	// Deliberately not t.Parallel(): mutates the package-level
	// claimStaleAfter (see TestStage_OrphanedClaimDoesNotBlockRetry).
	defer swapClaimStaleAfter(50 * time.Millisecond)()

	base := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	transferID := testutil.ID("xfer-clock-skew")
	w1, w2, t1 := testutil.ID("w1"), testutil.ID("w2"), testutil.ID("t1")

	openWallet(t, base, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, base, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, base, lc, w1, t1, usd(1000))
	fundToken(t, lc, t1, 1000)

	server := NewServer(base, lc, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, base, transferID, w1, w2, usd(400))

	if _, won, err := server.claimForDispatch(ctx, transferID, statePrepared, &pb.StagingTransferStarted{Id: transferID}); err != nil || !won {
		t.Fatalf("claimForDispatch() = (won=%v, err=%v), want (true, nil)", won, err)
	}

	// Real time passes well beyond claimStaleAfter...
	time.Sleep(4 * claimStaleAfter)

	// ...but a store whose clock lags an hour behind the one that stamped
	// the marker must still see it as fresh.
	behindStore := &skewedClockStore{Store: base, skew: -time.Hour}
	behindServer := NewServer(behindStore, lc, nil, nil)

	claimCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		won bool
		err error
	}
	claimDone := make(chan result, 1)
	go func() {
		_, won, err := behindServer.claimForDispatch(claimCtx, transferID, statePrepared, &pb.StagingTransferStarted{Id: transferID})
		claimDone <- result{won, err}
	}()

	select {
	case r := <-claimDone:
		t.Fatalf("claimForDispatch() on a clock lagging 1h returned (won=%v, err=%v) instead of still waiting on a marker real time already aged past claimStaleAfter", r.won, r.err)
	case <-time.After(10 * claimPollInterval):
		// Still waiting, as expected.
	}

	cancel()
	select {
	case r := <-claimDone:
		if r.won {
			t.Error("claimForDispatch() won after cancellation; it should have been blocked on the stale check, never raced ahead")
		}
	case <-time.After(time.Second):
		t.Fatal("claimForDispatch() never returned after ctx cancellation")
	}
}

// seedAbandonedClaim lands a Transfer at statePrepared with marker appended
// on top and nothing after it — a claim whose winner crashed mid-step — then
// waits until the marker is stale. Callers must have shrunk claimStaleAfter.
func seedAbandonedClaim(t *testing.T, store eventstore.Store, lc ledger.Client, transferID string, marker proto.Message) *Server {
	t.Helper()
	w1, w2, t1 := testutil.ID("w1"), testutil.ID("w2"), testutil.ID("t1")
	openWallet(t, store, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, w1, t1, usd(1000))
	fundToken(t, lc, t1, 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()
	seedPreparedTransfer(t, ctx, server, store, transferID, w1, w2, usd(400))

	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := store.Append(ctx, AggregateType, transferID, int64(len(events)), marker); err != nil {
		t.Fatalf("seed abandoned claim: %v", err)
	}
	time.Sleep(2 * claimStaleAfter)
	return server
}

// An abandoned claim can only be taken over by the same transition
// (go/docs/adr/0005): the crashed winner may have already half-applied its
// step — some chains voided in TigerBeetle, say — and a different transition's side
// effects on top of that are the contradiction this decision prevents.
// stage() over an abandoned cancel claim must refuse, loudly, without
// touching TigerBeetle or the stream.
func TestStage_RefusesStaleTakeoverOfADifferentTransition(t *testing.T) {
	// Deliberately not t.Parallel(): mutates claimStaleAfter.
	defer swapClaimStaleAfter(20 * time.Millisecond)()

	store := eventstore.NewMemoryStore()
	lc := &createTransfersCountingClient{Client: ledger.NewFakeClient()}
	transferID := testutil.ID("xfer-abandoned-cancel")
	server := seedAbandonedClaim(t, store, lc, transferID, &pb.CancellingPreparedTransferStarted{Id: transferID, Reason: "crashed"})
	lc.mu.Lock()
	lc.calls = 0
	lc.mu.Unlock()
	ctx := context.Background()
	before, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	err = server.stage(ctx, transferID)
	if !errors.Is(err, errAbandonedClaim) {
		t.Fatalf("stage() error = %v, want errAbandonedClaim", err)
	}
	var twerr twirp.Error
	if !errors.As(err, &twerr) || twerr.Code() != twirp.FailedPrecondition {
		t.Errorf("stage() error = %v, want a twirp FailedPrecondition", err)
	}
	if got := lc.count(); got != 0 {
		t.Errorf("ledger.CreateTransfers called %d times, want 0", got)
	}
	after, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("stream grew from %d to %d events; a refused takeover must append nothing", len(before), len(after))
	}
}

// The orchestrator is what recovers a crashed cancel: the marker's own
// publication triggers Resume, and runSaga must finish the claimed
// transition (cancel) rather than the one it would otherwise dispatch from
// Prepared (stage) — which would now be refused, halting the consumer.
func TestResume_FinishesAnAbandonedCancelPreparedClaim(t *testing.T) {
	// Deliberately not t.Parallel(): mutates claimStaleAfter.
	defer swapClaimStaleAfter(20 * time.Millisecond)()

	store := eventstore.NewMemoryStore()
	lc := &createTransfersCountingClient{Client: ledger.NewFakeClient()}
	transferID := testutil.ID("xfer-resume-cancel")
	server := seedAbandonedClaim(t, store, lc, transferID, &pb.CancellingPreparedTransferStarted{Id: transferID, Reason: "crashed"})
	lc.mu.Lock()
	lc.calls = 0
	lc.mu.Unlock()
	ctx := context.Background()

	if err := server.Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume() error = %v, want the abandoned cancel finished", err)
	}
	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if state := currentState(events); state != stateCancelled {
		t.Fatalf("state = %v, want cancelled", state)
	}
	if got := lc.count(); got != 0 {
		t.Errorf("ledger.CreateTransfers called %d times, want 0 (a Prepared cancel never touches TigerBeetle)", got)
	}
}

// The RPC face of the refusal: a client cancelling a Transfer whose staging
// crashed mid-step gets FailedPrecondition, not an Internal error and not a
// cancel layered over a half-applied stage.
func TestCancelAcceptedTransfer_RefusesOverAnAbandonedStageClaim(t *testing.T) {
	// Deliberately not t.Parallel(): mutates claimStaleAfter.
	defer swapClaimStaleAfter(20 * time.Millisecond)()

	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	transferID := testutil.ID("xfer-abandoned-stage")
	server := seedAbandonedClaim(t, store, lc, transferID, &pb.StagingTransferStarted{Id: transferID})

	_, err := server.CancelAcceptedTransfer(context.Background(), &pb.CancelAcceptedTransferRequest{Id: transferID, Reason: "client"})
	var twerr twirp.Error
	if !errors.As(err, &twerr) || twerr.Code() != twirp.FailedPrecondition {
		t.Fatalf("CancelAcceptedTransfer() error = %v, want a twirp FailedPrecondition", err)
	}
	events, err := store.Load(context.Background(), AggregateType, transferID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if state := currentState(events); state != statePrepared {
		t.Errorf("state = %v, want still prepared", state)
	}
}
