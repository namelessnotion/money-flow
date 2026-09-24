package transaction

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// appendRecordingStore records the event types of every append that landed on
// a Transaction's stream, one entry per append — so a test can see which
// facts shared a commit.
type appendRecordingStore struct {
	eventstore.Store
	mu      sync.Mutex
	appends [][]string
}

func (s *appendRecordingStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	err := s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	if err == nil && aggregateType == AggregateType {
		types := make([]string, len(events))
		for i, e := range events {
			types[i] = eventstore.EventType(e)
		}
		s.mu.Lock()
		s.appends = append(s.appends, types)
		s.mu.Unlock()
	}
	return err
}

// appendCarrying returns the append that recorded eventType, or nil.
func (s *appendRecordingStore) appendCarrying(eventType string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, types := range s.appends {
		if slices.Contains(types, eventType) {
			return types
		}
	}
	return nil
}

// The child outcome that decides a Transaction's own is recorded in the same
// append as that conclusion (go/docs/adr/0010): nothing else has to happen in
// between, so a second commit would only be another WAL flush.
func TestTransaction_TheLastChildsOutcomeAndTheConclusionShareOneAppend(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stage      bool
		rollback   bool
		childEvent proto.Message
		conclusion proto.Message
	}{
		"completed": {
			childEvent: &pb.TransferCompletedWithinTransaction{},
			conclusion: &pb.TransactionCompleted{},
		},
		"rolled back": {
			stage:      true,
			rollback:   true,
			childEvent: &pb.TransferRolledBackWithinTransaction{},
			conclusion: &pb.TransactionRolledBack{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := eventstore.NewMemoryStore()
			lc := ledger.NewFakeClient()
			from, to := testutil.ID("from"), testutil.ID("to")
			openWallet(t, base, from, sharedpb.Allows_ALLOWS_NONE)
			openWallet(t, base, to, sharedpb.Allows_ALLOWS_NONE)
			mintAndFundToken(t, base, lc, from, testutil.ID("from-token"), usd(10000))

			store := &appendRecordingStore{Store: base}
			xfers := newTransferServer(store, lc)
			txns := NewServer(store, xfers)
			ctx := context.Background()
			txnID, childID := testutil.ID("txn-"+name), testutil.ID("child-"+name)
			if _, err := txns.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
				Id: txnID,
				Transfers: map[string]*pb.Transfer{
					childID: {Id: childID, Amount: usd(10000), FromWalletId: from, ToWalletId: to, Stage: tc.stage},
				},
			}); err != nil {
				t.Fatalf("StartInitializingTransaction() error = %v", err)
			}
			driveSaga(t, txns, xfers, store, txnID)
			if tc.rollback {
				if _, err := txns.StartTransactionRollback(ctx, &pb.StartTransactionRollbackRequest{Id: txnID, Reason: "returned"}); err != nil {
					t.Fatalf("StartTransactionRollback() error = %v", err)
				}
				driveSaga(t, txns, xfers, store, txnID)
			}

			want := []string{eventstore.EventType(tc.childEvent), eventstore.EventType(tc.conclusion)}
			if got := store.appendCarrying(eventstore.EventType(tc.conclusion)); !slices.Equal(got, want) {
				t.Errorf("the append carrying %s was %v, want %v", want[1], got, want)
			}
		})
	}
}

// A child's failure decides that the Transaction rolls back, and nothing has
// to happen in between, so the rollback starts in the same append as the
// failure (go/docs/adr/0013). When the failed child is the only one that moved
// anything — it moved nothing — the rollback has nothing to undo and concludes
// in that append too.
func TestTransaction_AChildsFailureAndTheRollbackItStartsShareOneAppend(t *testing.T) {
	t.Parallel()
	failed := eventstore.EventType(&pb.TransferFailedWithinTransaction{})
	started := eventstore.EventType(&pb.TransactionRollbackStarted{})
	rolledBack := eventstore.EventType(&pb.TransactionRolledBack{})

	newWorld := func(t *testing.T) (*appendRecordingStore, *Server, *transfer.Server, string, string) {
		t.Helper()
		base := eventstore.NewMemoryStore()
		lc := ledger.NewFakeClient()
		bankAccount, cash, _, _ := achWallets(t, base)
		store := &appendRecordingStore{Store: base}
		xfers := newTransferServer(store, lc)
		return store, NewServer(store, xfers), xfers, bankAccount, cash
	}
	// A mint_source leg from a Wallet that was never opened passes the
	// accept-time pre-flight (mint_source has no balance to check) and is
	// rejected when requested.
	neverOpened := testutil.ID("never-opened")

	t.Run("nothing else to undo", func(t *testing.T) {
		t.Parallel()
		store, txns, xfers, _, cash := newWorld(t)
		ctx := context.Background()
		txnID, childID := testutil.ID("txn-alone"), testutil.ID("child-alone")
		if _, err := txns.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
			Id: txnID,
			Transfers: map[string]*pb.Transfer{
				childID: {Id: childID, Amount: usd(1000), FromWalletId: neverOpened, ToWalletId: cash, MintSource: true},
			},
		}); err != nil {
			t.Fatalf("StartInitializingTransaction() error = %v", err)
		}
		driveSaga(t, txns, xfers, store, txnID)

		if got, want := store.appendCarrying(started), []string{failed, started, rolledBack}; !slices.Equal(got, want) {
			t.Errorf("the append carrying %s was %v, want %v", started, got, want)
		}
		// Initialized, Started with the dispatch intent, then that one append.
		if got := len(store.appends); got != 3 {
			t.Errorf("the Transaction took %d appends, want 3: %v", got, store.appends)
		}

		events, err := store.Load(ctx, AggregateType, txnID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		reason, err := lastEventReason(events)
		if err != nil {
			t.Fatalf("lastEventReason() error = %v", err)
		}
		if !strings.Contains(reason, childID) {
			t.Errorf("TransactionRolledBack reason = %q, want it to name the child whose failure started the rollback", reason)
		}
	})

	t.Run("a committed sibling to reverse", func(t *testing.T) {
		t.Parallel()
		store, txns, xfers, bankAccount, cash := newWorld(t)
		ctx := context.Background()
		txnID, realID, shadowID := testutil.ID("txn-sibling"), testutil.ID("real"), testutil.ID("shadow")
		if _, err := txns.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
			Id: txnID,
			Transfers: map[string]*pb.Transfer{
				realID:   {Id: realID, Amount: usd(1000), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true},
				shadowID: {Id: shadowID, Amount: usd(1000), FromWalletId: neverOpened, ToWalletId: cash, MintSource: true},
			},
			TransferDependency: map[string]*pb.TransferIdList{shadowID: {TransferId: []string{realID}}},
		}); err != nil {
			t.Fatalf("StartInitializingTransaction() error = %v", err)
		}
		driveSaga(t, txns, xfers, store, txnID)

		// real committed, so the rollback has a Reversal to request before it
		// can conclude: it starts with the failure and ends later.
		if got, want := store.appendCarrying(started), []string{failed, started}; !slices.Equal(got, want) {
			t.Errorf("the append carrying %s was %v, want %v", started, got, want)
		}
		events, err := store.Load(ctx, AggregateType, txnID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if got := topLevelState(events); got != stateRolledBack {
			t.Fatalf("state = %v, want rolled_back; events = %v", got, eventTypesOf(events))
		}
	})
}

// Starting a Transaction is dispatching its first slice, so TransactionStarted
// is recorded in the same append as that slice's intents, against the fold
// that found it Initialized (go/docs/adr/0014). Only the first slice: later
// ones are the Transaction's own and carry no second start.
func TestRunSaga_StartsInTheSameAppendAsTheFirstSlice(t *testing.T) {
	t.Parallel()
	store := &appendRecordingStore{Store: eventstore.NewMemoryStore()}
	txns := NewServer(store, newAcceptingTransferClient())
	ctx := context.Background()

	requireSliceable(t)
	txnID := testutil.ID("txn-first-slice")
	seedInitialized(t, store, txnID, independentRoots(slicedChildren))
	started := eventstore.EventType(&pb.TransactionStarted{})
	requested := eventstore.EventType(&pb.TransferRequestedWithinTransaction{})

	if err := txns.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	first := store.appendCarrying(started)
	if len(first) != 1+maxDispatchPerStep || first[0] != started {
		t.Fatalf("the append carrying %s was %v, want it first and followed by one slice of %d intents",
			started, first, maxDispatchPerStep)
	}
	for _, eventType := range first[1:] {
		if eventType != requested {
			t.Errorf("the start's append carried %s, want only %s after it", eventType, requested)
		}
	}

	driveSaga(t, txns, nil, store, txnID)
	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	starts := 0
	for _, e := range events {
		if e.EventType == started {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("stream holds %d TransactionStarted, want exactly 1; events = %v", starts, eventTypesOf(events))
	}
	if got := len(childrenOf(t, store, txnID)); got != slicedChildren {
		t.Errorf("%d children requested once the saga settled, want all %d", got, slicedChildren)
	}
}
