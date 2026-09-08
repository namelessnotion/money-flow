package transfer

import (
	"context"
	"testing"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

func TestOwningTransaction_EmptyForUnknownID(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()

	got, err := OwningTransaction(context.Background(), store, testutil.ID("never-existed"))
	if err != nil {
		t.Fatalf("OwningTransaction() error = %v", err)
	}
	if got != "" {
		t.Errorf("OwningTransaction() = %q, want \"\"", got)
	}
}

func TestOwningTransaction_ReadsTransferRequestAccepted(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	transferID := testutil.ID("xfer1")
	txnID := testutil.ID("txn1")
	if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
		Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"),
		Amount: usd(400), TransactionId: txnID,
	}); err != nil {
		t.Fatalf("seed accepted: %v", err)
	}

	got, err := OwningTransaction(ctx, store, transferID)
	if err != nil {
		t.Fatalf("OwningTransaction() error = %v", err)
	}
	if got != txnID {
		t.Errorf("OwningTransaction() = %q, want %q", got, txnID)
	}
}

// A Transfer requested outside any Transaction is standalone: it has an
// owning-transaction answer, and that answer is "none".
func TestOwningTransaction_EmptyForStandaloneTransfer(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	transferID := testutil.ID("xfer-standalone")
	if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
		Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400),
	}); err != nil {
		t.Fatalf("seed accepted: %v", err)
	}

	got, err := OwningTransaction(ctx, store, transferID)
	if err != nil {
		t.Fatalf("OwningTransaction() error = %v", err)
	}
	if got != "" {
		t.Errorf("OwningTransaction() = %q, want \"\" for a standalone Transfer", got)
	}
}

// A Reversal is where this matters most: its terminal event is the only thing
// that can release a Transaction parked in rollback_started, so the link has
// to survive the Reversal being a separate aggregate of its own.
func TestOwningTransaction_ReadsReversalRequestAccepted(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	reversalID := testutil.ID("rev1")
	txnID := testutil.ID("txn1")
	if err := store.Append(ctx, AggregateType, reversalID, 0, &pb.ReversalRequestAccepted{
		Id: reversalID, TransferId: testutil.ID("xfer1"), Amount: usd(400), TransactionId: txnID,
	}); err != nil {
		t.Fatalf("seed reversal accepted: %v", err)
	}

	got, err := OwningTransaction(ctx, store, reversalID)
	if err != nil {
		t.Fatalf("OwningTransaction() error = %v", err)
	}
	if got != txnID {
		t.Errorf("OwningTransaction() = %q, want %q", got, txnID)
	}
}

// A rejected request never records a transaction_id — the Rejected events have
// no such field. That is not a gap: requestChildTransfer records the rejection
// onto the Transaction's own stream in the same call, so the Transaction learns
// of it through its own topic rather than through the child's.
func TestOwningTransaction_EmptyForRejectedRequest(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()

	for name, seed := range map[string]func(id string) error{
		"transfer": func(id string) error {
			return store.Append(ctx, AggregateType, id, 0, &pb.TransferRequestRejected{Id: id, Reason: "no capacity"})
		},
		"reversal": func(id string) error {
			return store.Append(ctx, AggregateType, id, 0, &pb.ReversalRequestRejected{Id: id, TransferId: testutil.ID("xfer1"), Reason: "not committed"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			transferID := testutil.ID("rejected-" + name)
			if err := seed(transferID); err != nil {
				t.Fatalf("seed rejected: %v", err)
			}
			got, err := OwningTransaction(ctx, store, transferID)
			if err != nil {
				t.Fatalf("OwningTransaction() error = %v", err)
			}
			if got != "" {
				t.Errorf("OwningTransaction() = %q, want \"\"", got)
			}
		})
	}
}

// The answer must not change as the saga advances: every later trigger on this
// Transfer has to resolve the same owning Transaction as the first one did.
func TestOwningTransaction_StableAcrossTheWholeStream(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	transferID := testutil.ID("xfer1")
	txnID := testutil.ID("txn1")
	if err := store.Append(ctx, AggregateType, transferID, 0,
		&pb.TransferRequestAccepted{
			Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"),
			Amount: usd(400), TransactionId: txnID,
		},
		&pb.TransferPrepared{Id: transferID, Legs: []*pb.TransferLeg{
			{SourceTokenId: testutil.ID("t1"), DestTokenId: testutil.ID("t2"), Amount: usd(400)},
		}},
		&pb.TransferCommitted{Id: transferID},
	); err != nil {
		t.Fatalf("seed stream: %v", err)
	}

	got, err := OwningTransaction(ctx, store, transferID)
	if err != nil {
		t.Fatalf("OwningTransaction() error = %v", err)
	}
	if got != txnID {
		t.Errorf("OwningTransaction() = %q, want %q", got, txnID)
	}
}
