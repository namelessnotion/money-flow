package saga

import (
	"context"
	"errors"
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// recordingResumer records which ids it was asked to resume, and can be made
// to fail, so routing can be observed without a store behind it.
type recordingResumer struct {
	resumed []string
	err     error
}

func (r *recordingResumer) Resume(_ context.Context, aggregateID string) error {
	r.resumed = append(r.resumed, aggregateID)
	return r.err
}

func noOwner(context.Context, string) (string, error) { return "", nil }

func ownedBy(transactionID string) OwnerResolver {
	return func(context.Context, string) (string, error) { return transactionID, nil }
}

func TestHandle_TransactionTriggerResumesTheTransaction(t *testing.T) {
	t.Parallel()
	transfers, transactions := &recordingResumer{}, &recordingResumer{}
	txnID := testutil.ID("txn1")

	err := New(transfers, transactions, noOwner).Handle(context.Background(), Trigger{
		AggregateType: transaction.AggregateType, AggregateID: txnID,
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(transactions.resumed) != 1 || transactions.resumed[0] != txnID {
		t.Errorf("transactions resumed = %v, want [%s]", transactions.resumed, txnID)
	}
	if len(transfers.resumed) != 0 {
		t.Errorf("transfers resumed = %v, want none", transfers.resumed)
	}
}

// The load-bearing half: a trigger names a Transfer, but whether the
// Transaction can now finish is a decision on the Transaction, so both get
// driven.
func TestHandle_TransferTriggerAlsoResumesTheOwningTransaction(t *testing.T) {
	t.Parallel()
	transfers, transactions := &recordingResumer{}, &recordingResumer{}
	transferID := testutil.ID("xfer1")
	txnID := testutil.ID("txn1")

	err := New(transfers, transactions, ownedBy(txnID)).Handle(context.Background(), Trigger{
		AggregateType: transfer.AggregateType, AggregateID: transferID,
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(transfers.resumed) != 1 || transfers.resumed[0] != transferID {
		t.Errorf("transfers resumed = %v, want [%s]", transfers.resumed, transferID)
	}
	if len(transactions.resumed) != 1 || transactions.resumed[0] != txnID {
		t.Errorf("transactions resumed = %v, want [%s]", transactions.resumed, txnID)
	}
}

func TestHandle_StandaloneTransferTouchesNoTransaction(t *testing.T) {
	t.Parallel()
	transfers, transactions := &recordingResumer{}, &recordingResumer{}
	transferID := testutil.ID("xfer-standalone")

	err := New(transfers, transactions, noOwner).Handle(context.Background(), Trigger{
		AggregateType: transfer.AggregateType, AggregateID: transferID,
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(transfers.resumed) != 1 {
		t.Errorf("transfers resumed = %v, want exactly the transfer", transfers.resumed)
	}
	if len(transactions.resumed) != 0 {
		t.Errorf("transactions resumed = %v, want none", transactions.resumed)
	}
}

// The Transfer is driven before its Transaction re-folds, so the Transaction
// sees the child as far along as this trigger can take it.
func TestHandle_ResumesTheTransferBeforeItsTransaction(t *testing.T) {
	t.Parallel()
	var order []string
	transferID := testutil.ID("xfer1")
	txnID := testutil.ID("txn1")

	transfers := resumerFunc(func(string) error { order = append(order, "transfer"); return nil })
	transactions := resumerFunc(func(string) error { order = append(order, "transaction"); return nil })

	err := New(transfers, transactions, ownedBy(txnID)).Handle(context.Background(), Trigger{
		AggregateType: transfer.AggregateType, AggregateID: transferID,
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(order) != 2 || order[0] != "transfer" || order[1] != "transaction" {
		t.Errorf("resume order = %v, want [transfer transaction]", order)
	}
}

func TestHandle_RejectsAnUnknownAggregateType(t *testing.T) {
	t.Parallel()

	err := New(&recordingResumer{}, &recordingResumer{}, noOwner).Handle(context.Background(), Trigger{
		AggregateType: "wallet", AggregateID: testutil.ID("w1"),
	})
	if err == nil {
		t.Fatal("Handle() = nil error for an unrouted aggregate type, want an error")
	}
}

// Failures have to reach the consumer, which is the only thing that can decide
// whether to retry or halt.
func TestHandle_SurfacesFailures(t *testing.T) {
	t.Parallel()
	boom := errors.New("store unreachable")
	txnID := testutil.ID("txn1")

	t.Run("from the transfer", func(t *testing.T) {
		t.Parallel()
		transactions := &recordingResumer{}
		err := New(&recordingResumer{err: boom}, transactions, ownedBy(txnID)).Handle(context.Background(), Trigger{
			AggregateType: transfer.AggregateType, AggregateID: testutil.ID("xfer1"),
		})
		if !errors.Is(err, boom) {
			t.Errorf("Handle() error = %v, want %v", err, boom)
		}
		if len(transactions.resumed) != 0 {
			t.Errorf("transactions resumed = %v; a failed child must not be reported upward as progress", transactions.resumed)
		}
	})

	t.Run("from the owner lookup", func(t *testing.T) {
		t.Parallel()
		owner := func(context.Context, string) (string, error) { return "", boom }
		err := New(&recordingResumer{}, &recordingResumer{}, owner).Handle(context.Background(), Trigger{
			AggregateType: transfer.AggregateType, AggregateID: testutil.ID("xfer1"),
		})
		if !errors.Is(err, boom) {
			t.Errorf("Handle() error = %v, want %v", err, boom)
		}
	})

	t.Run("from the transaction", func(t *testing.T) {
		t.Parallel()
		err := New(&recordingResumer{}, &recordingResumer{err: boom}, noOwner).Handle(context.Background(), Trigger{
			AggregateType: transaction.AggregateType, AggregateID: txnID,
		})
		if !errors.Is(err, boom) {
			t.Errorf("Handle() error = %v, want %v", err, boom)
		}
	})
}

// resumerFunc adapts a plain function to Resumer.
type resumerFunc func(aggregateID string) error

func (f resumerFunc) Resume(_ context.Context, aggregateID string) error { return f(aggregateID) }
