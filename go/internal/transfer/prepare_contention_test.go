package transfer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// contendedWalletStore lands a genuine competing write on walletID — another
// Token minted into it — just before each of the first n AppendAtomic calls,
// the way a sibling Transfer into the same Wallet prepares a moment earlier on
// another partition. The call itself then reaches the real store with the
// Wallet's now-stale ExpectedSeq, and loses for real.
type contendedWalletStore struct {
	eventstore.Store
	t        *testing.T
	lc       ledger.Client
	walletID string
	n        int
	landed   int
	onLanded func(landed int)
}

func (s *contendedWalletStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	if s.landed < s.n {
		s.landed++
		mintToken(s.t, s.Store, s.lc, s.walletID, testutil.ID(fmt.Sprintf("sibling-%d", s.landed)), usd(1))
		if s.onLanded != nil {
			s.onLanded(s.landed)
		}
	}
	return s.Store.AppendAtomic(ctx, writes...)
}

func acceptedTransferInto(t *testing.T, store eventstore.Store, lc ledger.Client) (from, to, transferID string) {
	t.Helper()
	from, to, transferID = testutil.ID("w1"), testutil.ID("w2"), testutil.ID("xfer1")
	openWallet(t, store, from, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, from, testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	resp, err := NewServer(store, lc, nil, nil).RequestTransfer(context.Background(),
		transferRequest(transferID, from, to, usd(400), true))
	if err != nil || resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("RequestTransfer() = %v, %v; want Accepted", resp.GetResult(), err)
	}
	return from, to, transferID
}

// Losing the destination Wallet to a sibling Transfer is contention, not a
// fault: the Tokens this Transfer mints are still its own to mint, only the
// Wallet's position moved. Prepare re-plans against the Wallet as it now
// stands rather than handing the orchestrator an error to back off on — and,
// under enough of it, halt over (go/docs/adr/0003).
func TestPrepare_ReplansWhenASiblingTransferMovesTheDestinationWallet(t *testing.T) {
	t.Parallel()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	_, to, transferID := acceptedTransferInto(t, store, lc)

	contended := &contendedWalletStore{Store: store, t: t, lc: lc, walletID: to, n: 2}
	if err := NewServer(contended, lc, nil, nil).Resume(context.Background(), transferID); err != nil {
		t.Fatalf("Resume() error = %v, want prepare to re-plan past two siblings landing first", err)
	}
	if state := currentState(mustEvents(t, store, transferID)); state != stateStaged {
		t.Errorf("state = %v, want staged", state)
	}
}

// preparingRounds runs every prepare of a hot Wallet in lock-step rounds:
// each prepare's AppendAtomic waits until every Transfer still preparing has
// planned against the Wallet as it stands, then all of them append at once.
// Exactly one lands per round and every other one loses for real, so the
// last of n Transfers to land loses n-1 times — the burst a hot Wallet sees
// when as many partitions as the orchestrator has prepare against it at once.
type preparingRounds struct {
	eventstore.Store
	mu        sync.Mutex
	cond      *sync.Cond
	remaining int
	arrived   int
	round     int
	conflicts int
}

func newPreparingRounds(store eventstore.Store, contenders int) *preparingRounds {
	s := &preparingRounds{Store: store, remaining: contenders}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *preparingRounds) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	if !preparesATransfer(writes) {
		return s.Store.AppendAtomic(ctx, writes...)
	}

	s.mu.Lock()
	s.arrived++
	round := s.round
	s.releaseIfEveryoneArrived()
	for s.round == round {
		s.cond.Wait()
	}
	s.mu.Unlock()

	err := s.Store.AppendAtomic(ctx, writes...)
	if errors.Is(err, eventstore.ErrConcurrencyConflict) {
		s.mu.Lock()
		s.conflicts++
		s.mu.Unlock()
	}
	return err
}

// finished takes one Transfer out of the rounds, however its prepare ended.
func (s *preparingRounds) finished() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining--
	s.releaseIfEveryoneArrived()
}

func (s *preparingRounds) releaseIfEveryoneArrived() {
	if s.arrived > 0 && s.arrived == s.remaining {
		s.round++
		s.arrived = 0
		s.cond.Broadcast()
	}
}

func preparesATransfer(writes []eventstore.StreamWrite) bool {
	for _, w := range writes {
		for _, e := range w.Events {
			if _, ok := e.(*pb.TransferPrepared); ok {
				return true
			}
		}
	}
	return false
}

// A hot Wallet is contention, not a fault, however hot it gets. Here it is a
// reserve every Transfer mints its source from — cmd/simulate's seed burst,
// which halted the orchestrator at 24 partitions — and the last of 24
// Transfers loses the reserve to every one of the other 23. Each of those
// losses means another Transfer's prepare landed, so the burst drains; none
// of it is handed to the orchestrator to retry into a halt.
func TestPrepare_ConvergesHoweverManyTransfersContendForOneWallet(t *testing.T) {
	t.Parallel()
	const contenders = 24
	ctx := context.Background()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	exists := func(context.Context, string) (bool, error) { return true, nil }

	reserve := testutil.ID("reserve")
	openWallet(t, store, reserve, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	transferIDs := make([]string, contenders)
	for i := range transferIDs {
		to, transferID := testutil.ID(fmt.Sprintf("entity-%d", i)), testutil.ID(fmt.Sprintf("seed-%d", i))
		openWallet(t, store, to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
		req := transferRequest(transferID, reserve, to, usd(100), true)
		req.TransactionId, req.MintSource = testutil.ID(fmt.Sprintf("seed-tx-%d", i)), true
		resp, err := NewServer(store, lc, nil, exists).RequestTransfer(ctx, req)
		if err != nil || resp.GetTransferRequestAccepted() == nil {
			t.Fatalf("RequestTransfer(%d) = %v, %v; want Accepted", i, resp.GetResult(), err)
		}
		transferIDs[i] = transferID
	}

	rounds := newPreparingRounds(store, contenders)
	server := NewServer(rounds, lc, nil, exists)
	errs := make([]error, contenders)
	var wg sync.WaitGroup
	for i, transferID := range transferIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer rounds.finished()
			errs[i] = server.Resume(ctx, transferID)
		}()
	}
	wg.Wait()

	for i, transferID := range transferIDs {
		if errs[i] != nil {
			t.Errorf("Resume(seed-%d) error = %v, want every prepare to land eventually", i, errs[i])
			continue
		}
		if state := currentState(mustEvents(t, store, transferID)); state != stateStaged {
			t.Errorf("seed-%d state = %v, want staged", i, state)
		}
	}
	if want := contenders * (contenders - 1) / 2; rounds.conflicts != want {
		t.Errorf("prepares lost %d races for the reserve, want %d; the rounds did not contend as designed",
			rounds.conflicts, want)
	}
}

// Re-planning is unbounded only because every loss is paid for by someone
// else's landed write. A conflict that nothing landed to cause is not
// contention — nothing is draining, so trying again would spin — and goes
// back to the driver as the fault it is, with nothing half-written.
func TestPrepare_ReportsAConflictNothingMovedFor(t *testing.T) {
	t.Parallel()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	_, _, transferID := acceptedTransferInto(t, store, lc)

	phantom := &phantomConflictStore{Store: store}
	err := NewServer(phantom, lc, nil, nil).Resume(context.Background(), transferID)
	if err == nil {
		t.Fatal("Resume() error = nil, want a conflict nothing moved for reported as a fault")
	}
	if errors.Is(err, errPlanOvertaken) {
		t.Errorf("Resume() error = %v, want it not to be treated as contention", err)
	}
	if phantom.calls != 1 {
		t.Errorf("prepare tried %d times, want 1: nothing moved, so re-planning cannot change the outcome", phantom.calls)
	}
	if state := currentState(mustEvents(t, store, transferID)); state != stateAccepted {
		t.Errorf("state = %v, want still accepted", state)
	}
}

// phantomConflictStore refuses every AppendAtomic as a concurrency conflict
// without anything having landed.
type phantomConflictStore struct {
	eventstore.Store
	calls int
}

func (s *phantomConflictStore) AppendAtomic(context.Context, ...eventstore.StreamWrite) error {
	s.calls++
	return eventstore.ErrConcurrencyConflict
}

// With no bound on re-planning, the driver's context is what ends it: a
// shutting-down orchestrator is not held up by a Wallet that keeps moving.
func TestPrepare_StopsReplanningWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	_, to, transferID := acceptedTransferInto(t, store, lc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	contended := &contendedWalletStore{
		Store: store, t: t, lc: lc, walletID: to, n: 1 << 30,
		onLanded: func(landed int) {
			if landed == 5 {
				cancel()
			}
		},
	}
	err := NewServer(contended, lc, nil, nil).Resume(ctx, transferID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resume() error = %v, want context.Canceled", err)
	}
	if state := currentState(mustEvents(t, store, transferID)); state != stateAccepted {
		t.Errorf("state = %v, want still accepted", state)
	}
}

// A cancel that lands while prepare is planning moves the Transfer's own
// stream, which loses prepare the race exactly as a sibling on the Wallet
// would. The re-plan must see the cancel and stop, not prepare — and go on to
// move money for — a Transfer that is already cancelled.
func TestPrepare_DoesNotPrepareATransferCancelledWhileItPlanned(t *testing.T) {
	t.Parallel()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	_, _, transferID := acceptedTransferInto(t, store, lc)

	cancelled := &cancelFirstStore{Store: store, cancel: func() {
		resp, err := NewServer(store, lc, nil, nil).CancelAcceptedTransfer(context.Background(),
			&pb.CancelAcceptedTransferRequest{Id: transferID, Reason: "changed my mind"})
		if err != nil || resp.GetAcceptedTransferCancelled() == nil {
			t.Errorf("CancelAcceptedTransfer() = %v, %v; want Cancelled", resp.GetResult(), err)
		}
	}}
	if err := NewServer(cancelled, lc, nil, nil).Resume(context.Background(), transferID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	events := mustEvents(t, store, transferID)
	if state := currentState(events); state != stateCancelled {
		t.Errorf("state = %v, want cancelled", state)
	}
	for _, e := range events {
		if e.EventType == eventstore.EventType(&pb.TransferPrepared{}) {
			t.Errorf("stream has %s after the cancel; a cancelled Transfer was prepared", e.EventType)
		}
	}
}

// cancelFirstStore runs cancel just before the first AppendAtomic.
type cancelFirstStore struct {
	eventstore.Store
	cancel func()
}

func (s *cancelFirstStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	if s.cancel != nil {
		cancel := s.cancel
		s.cancel = nil
		cancel()
	}
	return s.Store.AppendAtomic(ctx, writes...)
}

// A lost plan leaves nothing in the ledger. Minting creates the Token's
// TigerBeetle account before the append that can lose, and that account
// outlives the lost append; re-planning reuses the Token ids it chose the
// first time, so a Transfer that loses its Wallet many times over still owns
// exactly the accounts it would have had it won at once.
func TestPrepare_ReplansWithoutLeavingLedgerAccountsBehind(t *testing.T) {
	t.Parallel()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	_, to, transferID := acceptedTransferInto(t, store, lc)

	counting := &accountCountingLedger{Client: lc, ids: map[string]bool{}}
	contended := &contendedWalletStore{Store: store, t: t, lc: lc, walletID: to, n: 5}
	if err := NewServer(contended, counting, nil, nil).Resume(context.Background(), transferID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if len(counting.ids) != 1 {
		t.Errorf("prepare created %d ledger accounts across its re-plans, want 1 (the destination Token)", len(counting.ids))
	}
}

// accountCountingLedger records every distinct account id asked to be created.
type accountCountingLedger struct {
	ledger.Client
	ids map[string]bool
}

func (l *accountCountingLedger) CreateAccounts(ctx context.Context, accounts []ledger.Account) ([]ledger.AccountResult, error) {
	for _, a := range accounts {
		l.ids[a.ID] = true
	}
	return l.Client.CreateAccounts(ctx, accounts)
}
