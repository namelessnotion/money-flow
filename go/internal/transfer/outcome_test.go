package transfer

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

func TestOutcome_NotFoundForUnknownID(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()

	got, err := Outcome(context.Background(), store, testutil.ID("never-existed"))
	if err != nil {
		t.Fatalf("Outcome() error = %v", err)
	}
	if got != OutcomeNotFound {
		t.Errorf("Outcome() = %v, want OutcomeNotFound", got)
	}
}

func TestOutcome_Rejected(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	transferID := testutil.ID("xfer1")
	if err := store.Append(ctx, AggregateType, transferID, 0,
		&pb.TransferRequestRejected{Id: transferID, Reason: "insufficient capacity"},
	); err != nil {
		t.Fatalf("seed rejected: %v", err)
	}

	got, err := Outcome(ctx, store, transferID)
	if err != nil {
		t.Fatalf("Outcome() error = %v", err)
	}
	if got != OutcomeRejected {
		t.Errorf("Outcome() = %v, want OutcomeRejected", got)
	}
}

func TestOutcome_ReversalRejected(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	reversalID := testutil.ID("rev1")
	if err := store.Append(ctx, AggregateType, reversalID, 0,
		&pb.ReversalRequestRejected{Id: reversalID, TransferId: testutil.ID("xfer1"), Reason: "not committed"},
	); err != nil {
		t.Fatalf("seed rejected: %v", err)
	}

	got, err := Outcome(ctx, store, reversalID)
	if err != nil {
		t.Fatalf("Outcome() error = %v", err)
	}
	if got != OutcomeRejected {
		t.Errorf("Outcome() = %v, want OutcomeRejected", got)
	}
}

func TestOutcome_InFlightForAcceptedAndPrepared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		store := eventstore.NewMemoryStore()
		transferID := testutil.ID("xfer-accepted")
		if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
			Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400),
		}); err != nil {
			t.Fatalf("seed accepted: %v", err)
		}
		got, err := Outcome(ctx, store, transferID)
		if err != nil {
			t.Fatalf("Outcome() error = %v", err)
		}
		if got != OutcomeInFlight {
			t.Errorf("Outcome() = %v, want OutcomeInFlight", got)
		}
	})

	t.Run("prepared", func(t *testing.T) {
		t.Parallel()
		store := eventstore.NewMemoryStore()
		transferID := testutil.ID("xfer-prepared")
		if err := store.Append(ctx, AggregateType, transferID, 0,
			&pb.TransferRequestAccepted{Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400)},
			&pb.TransferPrepared{Id: transferID, Legs: []*pb.TransferLeg{
				{SourceTokenId: testutil.ID("t1"), DestTokenId: testutil.ID("t2"), Amount: usd(400)},
			}},
		); err != nil {
			t.Fatalf("seed prepared: %v", err)
		}
		got, err := Outcome(ctx, store, transferID)
		if err != nil {
			t.Fatalf("Outcome() error = %v", err)
		}
		if got != OutcomeInFlight {
			t.Errorf("Outcome() = %v, want OutcomeInFlight", got)
		}
	})
}

func TestOutcome_MirrorsSagaProgression(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, lc, nil, nil)
	ctx := context.Background()

	// Staged: OutcomeStaged, then OutcomePending, then OutcomeCommitted.
	transferID := testutil.ID("xfer1")
	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, testutil.ID("w1"), testutil.ID("w2"), usd(400), true)); err != nil {
		t.Fatalf("RequestTransfer(stage=true) error = %v", err)
	}
	if got, err := Outcome(ctx, store, transferID); err != nil || got != OutcomeStaged {
		t.Fatalf("Outcome() = (%v, %v), want OutcomeStaged", got, err)
	}

	if _, err := server.ConfirmStagedTransfer(ctx, &pb.ConfirmStagedTransferRequest{Id: transferID}); err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if got, err := Outcome(ctx, store, transferID); err != nil || got != OutcomePending {
		t.Fatalf("Outcome() = (%v, %v), want OutcomePending", got, err)
	}

	if _, err := server.PostPendingTransfer(ctx, &pb.PostPendingTransferRequest{Id: transferID}); err != nil {
		t.Fatalf("PostPendingTransfer() error = %v", err)
	}
	if got, err := Outcome(ctx, store, transferID); err != nil || got != OutcomeCommitted {
		t.Fatalf("Outcome() = (%v, %v), want OutcomeCommitted", got, err)
	}
}

func TestOutcome_Cancelled(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	ctx := context.Background()
	transferID := testutil.ID("xfer1")
	if err := store.Append(ctx, AggregateType, transferID, 0, &pb.TransferRequestAccepted{
		Id: transferID, FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"), Amount: usd(400),
	}); err != nil {
		t.Fatalf("seed accepted: %v", err)
	}

	server := NewServer(store, nil, nil, nil)
	if _, err := server.CancelAcceptedTransfer(ctx, &pb.CancelAcceptedTransferRequest{Id: transferID, Reason: "changed my mind"}); err != nil {
		t.Fatalf("CancelAcceptedTransfer() error = %v", err)
	}

	got, err := Outcome(ctx, store, transferID)
	if err != nil {
		t.Fatalf("Outcome() error = %v", err)
	}
	if got != OutcomeCancelled {
		t.Errorf("Outcome() = %v, want OutcomeCancelled", got)
	}
}

func TestOutcome_Failed(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	openWallet(t, store, testutil.ID("w1"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, testutil.ID("w2"), sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, testutil.ID("w1"), testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	server := NewServer(store, &rejectingTransfersClient{Client: lc}, nil, nil)
	ctx := context.Background()
	transferID := testutil.ID("xfer1")
	if _, err := requestAndRun(t, server, ctx, transferRequest(transferID, testutil.ID("w1"), testutil.ID("w2"), usd(400), false)); err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}

	got, err := Outcome(ctx, store, transferID)
	if err != nil {
		t.Fatalf("Outcome() error = %v", err)
	}
	if got != OutcomeFailed {
		t.Errorf("Outcome() = %v, want OutcomeFailed", got)
	}
}
