package transaction

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// A Transaction initialized but never advanced — what an external driver finds
// once nothing runs the saga in the initializing call — is started and its
// ready children dispatched by Resume alone.
func TestResume_StartsAnInitializedTransaction(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	uncleared := testutil.ID("uncleared")
	cleared := testutil.ID("cleared")
	openWallet(t, store, uncleared, sharedpb.Allows_ALLOWS_NONE)
	openWallet(t, store, cleared, sharedpb.Allows_ALLOWS_NONE)
	mintAndFundToken(t, store, lc, uncleared, testutil.ID("uncleared-token"), usd(10000))

	ctx := context.Background()
	txnID := testutil.ID("txn1")
	clearID := testutil.ID("clear")
	if err := store.Append(ctx, AggregateType, txnID, 0, &pb.TransactionInitialized{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			clearID: {Id: clearID, Amount: usd(10000), FromWalletId: uncleared, ToWalletId: cleared, AutoProcess: true},
		},
	}); err != nil {
		t.Fatalf("seed initialized: %v", err)
	}

	server := NewServer(store, newTransferServer(store, lc))
	if err := server.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateCompleted {
		t.Fatalf("state = %v, want completed", got)
	}
}

// Redelivery must change neither state nor event count.
func TestResume_IsANoOpOnATerminalTransaction(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	uncleared := testutil.ID("uncleared")
	cleared := testutil.ID("cleared")
	openWallet(t, store, uncleared, sharedpb.Allows_ALLOWS_NONE)
	openWallet(t, store, cleared, sharedpb.Allows_ALLOWS_NONE)
	mintAndFundToken(t, store, lc, uncleared, testutil.ID("uncleared-token"), usd(10000))

	server := NewServer(store, newTransferServer(store, lc))
	ctx := context.Background()
	txnID := testutil.ID("txn1")
	clearID := testutil.ID("clear")
	if _, err := server.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			clearID: {Id: clearID, Amount: usd(10000), FromWalletId: uncleared, ToWalletId: cleared, AutoProcess: true},
		},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	before, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if err := server.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	after, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("stream grew from %d to %d events; Resume must be a no-op here", len(before), len(after))
	}
}

func TestResume_ErrorsOnAnUnknownID(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, newTransferServer(store, ledger.NewFakeClient()))

	if err := server.Resume(context.Background(), testutil.ID("never-existed")); err == nil {
		t.Error("Resume() on an unknown id = nil, want an error")
	}
}
