package token

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// recordingRounds runs every recording of one hot Token in lock-step rounds:
// each Append of its balance waits until every contender still recording has
// read the stream and the ledger as they stand, then all of them append at
// once. Exactly one lands per round and every other one loses for real.
//
// Just before a contended round appends, onRound lands another write on the
// Token's ledger account — the next Transfer on some partition staging its
// debit. So the losers' re-read never matches what just landed, and none of
// them can bow out as already recorded: the last of n contenders loses n-1
// times, the sustained debiting a hot source Token sees when as many
// partitions as the orchestrator has all draw on it.
type recordingRounds struct {
	eventstore.Store
	tokenID   string
	onRound   func()
	mu        sync.Mutex
	cond      *sync.Cond
	remaining int
	arrived   int
	round     int
	conflicts int
}

func newRecordingRounds(store eventstore.Store, tokenID string, contenders int, onRound func()) *recordingRounds {
	s := &recordingRounds{Store: store, tokenID: tokenID, onRound: onRound, remaining: contenders}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *recordingRounds) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	if aggregateType != AggregateType || aggregateID != s.tokenID {
		return s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	}

	s.mu.Lock()
	s.arrived++
	round := s.round
	s.releaseIfEveryoneArrived()
	for s.round == round {
		s.cond.Wait()
	}
	s.mu.Unlock()

	err := s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	if errors.Is(err, eventstore.ErrConcurrencyConflict) {
		s.mu.Lock()
		s.conflicts++
		s.mu.Unlock()
	}
	return err
}

// finished takes one contender out of the rounds, however its recording
// ended.
func (s *recordingRounds) finished() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining--
	s.releaseIfEveryoneArrived()
}

func (s *recordingRounds) releaseIfEveryoneArrived() {
	if s.arrived > 0 && s.arrived == s.remaining {
		if s.arrived > 1 {
			s.onRound()
		}
		s.round++
		s.arrived = 0
		s.cond.Broadcast()
	}
}

// A hot source Token is contention, not a fault, however hot it gets. Every
// lost race means another recording landed, so the burst drains; none of it
// is handed back to the saga step to retry into an orchestrator halt
// (go/docs/adr/0003). And the balance that lands last is the ledger's latest,
// not a stale read that happened to win.
func TestRecordBalances_ConvergesHoweverManyTransfersDebitOneToken(t *testing.T) {
	t.Parallel()
	const contenders = 24
	ctx := context.Background()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	bank := mintedToken(t, store, lc, "bank", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	source := mintedToken(t, store, lc, "source", sharedpb.Allows_ALLOWS_NONE)
	dest := mintedToken(t, store, lc, "dest", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	move(t, lc, "fund", bank, source, 1_000_000, ledger.TransferKindRegular)

	debits := 0
	// onRound runs on a contender's goroutine, holding the rounds' lock, so
	// it reports rather than calling t.Fatal.
	rounds := newRecordingRounds(store, source, contenders, func() {
		debits++
		results, err := lc.CreateTransfers(ctx, []ledger.Transfer{{
			ID: testutil.ID(fmt.Sprintf("sibling-%d", debits)), DebitAccountID: source, CreditAccountID: dest,
			MinorUnits: 1, Currency: "USD", Kind: ledger.TransferKindPending, Timeout: 3600,
		}})
		if err != nil || results[0].Result != ledger.TransferResultOK {
			t.Errorf("sibling debit %d: %v %v", debits, results, err)
		}
	})
	errs := make([]error, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer rounds.finished()
			errs[i] = RecordBalances(ctx, rounds, lc, []string{source})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("RecordBalances(contender %d) error = %v, want every recording to land eventually", i, err)
		}
	}
	if want := contenders * (contenders - 1) / 2; rounds.conflicts != want {
		t.Errorf("recordings lost %d races for the Token, want %d; the rounds did not contend as designed",
			rounds.conflicts, want)
	}
	got := recorded(t, store, source)
	if len(got) == 0 || got[len(got)-1].GetPendingOutgoingMinorUnits() != uint64(debits) {
		t.Errorf("last recorded = %v, want pending outgoing %d: the ledger's latest must land last", got, debits)
	}
}

// Retrying is unbounded only because every loss is paid for by someone
// else's landed recording. A conflict that nothing landed to cause is not
// contention — nothing is draining, so trying again would spin — and goes
// back to the saga step as the fault it is.
func TestRecordBalances_ReportsAConflictNothingMovedFor(t *testing.T) {
	t.Parallel()
	inner := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bank := mintedToken(t, inner, lc, "bank", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	cash := mintedToken(t, inner, lc, "cash", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	move(t, lc, "posted", bank, cash, 300, ledger.TransferKindRegular)

	phantom := &phantomConflictStore{Store: inner}
	if err := RecordBalances(context.Background(), phantom, lc, []string{cash}); err == nil {
		t.Fatal("RecordBalances() error = nil, want a conflict nothing moved for reported as a fault")
	}
	if phantom.calls != 1 {
		t.Errorf("recording tried %d times, want 1: nothing moved, so trying again cannot change the outcome", phantom.calls)
	}
}

// phantomConflictStore refuses every Append as a concurrency conflict
// without anything having landed.
type phantomConflictStore struct {
	eventstore.Store
	calls int
}

func (s *phantomConflictStore) Append(context.Context, string, string, int64, ...proto.Message) error {
	s.calls++
	return eventstore.ErrConcurrencyConflict
}

// With no bound on retrying, the saga step's context is what ends it: a
// shutting-down orchestrator is not held up by a Token that keeps moving.
func TestRecordBalances_StopsRetryingWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	inner := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bank := mintedToken(t, inner, lc, "bank", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	cash := mintedToken(t, inner, lc, "cash", sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	moving := &everMovingStore{Store: inner, onConflict: func(n int) {
		if err := RecordBalances(context.Background(), inner, lc, []string{cash}); err != nil {
			t.Errorf("sibling RecordBalances: %v", err)
		}
		move(t, lc, fmt.Sprintf("sibling-%d", n), bank, cash, 1, ledger.TransferKindRegular)
		if n == 5 {
			cancel()
		}
	}}
	if err := RecordBalances(ctx, moving, lc, []string{cash}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordBalances() error = %v, want context.Canceled", err)
	}
}

// everMovingStore loses every Append to a real one: onConflict lands a
// sibling's recording first, then the next sibling Transfer's ledger write,
// so the loser's re-read never matches what just landed. The Append then
// reaches the store with the now-stale position.
type everMovingStore struct {
	eventstore.Store
	onConflict func(n int)
	n          int
}

func (s *everMovingStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	s.n++
	s.onConflict(s.n)
	return s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
}
