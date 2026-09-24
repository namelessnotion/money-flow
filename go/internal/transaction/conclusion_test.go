package transaction

import (
	"context"
	"slices"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
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
					childID: {Id: childID, Amount: usd(10000), FromWalletId: from, ToWalletId: to, AutoProcess: true, Stage: tc.stage},
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
