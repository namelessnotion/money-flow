package main

import (
	"context"
	"testing"

	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
)

// scriptedTransferService implements transferpb.TransferService for
// driveOneTransfer/settleTransfer's own tests: each method delegates to a
// function field a test sets, and an unset field panics — the same
// "unscripted call is a test bug" contract fakeTransfers uses elsewhere in
// this package. CancelAcceptedTransfer and RequestReversal are never called
// by driveOneTransfer/settleTransfer, so no test ever needs to script them.
type scriptedTransferService struct {
	requestTransfer       func(*transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error)
	confirmStagedTransfer func(*transferpb.ConfirmStagedTransferRequest) (*transferpb.ConfirmStagedTransferResponse, error)
	cancelStagedTransfer  func(*transferpb.CancelStagedTransferRequest) (*transferpb.CancelStagedTransferResponse, error)
	postPendingTransfer   func(*transferpb.PostPendingTransferRequest) (*transferpb.PostPendingTransferResponse, error)
}

func (f *scriptedTransferService) RequestTransfer(_ context.Context, req *transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error) {
	if f.requestTransfer == nil {
		panic("scriptedTransferService: RequestTransfer not scripted")
	}
	return f.requestTransfer(req)
}

func (f *scriptedTransferService) CancelAcceptedTransfer(context.Context, *transferpb.CancelAcceptedTransferRequest) (*transferpb.CancelAcceptedTransferResponse, error) {
	panic("scriptedTransferService: CancelAcceptedTransfer not scripted")
}

func (f *scriptedTransferService) RequestReversal(context.Context, *transferpb.RequestReversalRequest) (*transferpb.RequestReversalResponse, error) {
	panic("scriptedTransferService: RequestReversal not scripted")
}

func (f *scriptedTransferService) ConfirmStagedTransfer(_ context.Context, req *transferpb.ConfirmStagedTransferRequest) (*transferpb.ConfirmStagedTransferResponse, error) {
	if f.confirmStagedTransfer == nil {
		panic("scriptedTransferService: ConfirmStagedTransfer not scripted")
	}
	return f.confirmStagedTransfer(req)
}

func (f *scriptedTransferService) CancelStagedTransfer(_ context.Context, req *transferpb.CancelStagedTransferRequest) (*transferpb.CancelStagedTransferResponse, error) {
	if f.cancelStagedTransfer == nil {
		panic("scriptedTransferService: CancelStagedTransfer not scripted")
	}
	return f.cancelStagedTransfer(req)
}

func (f *scriptedTransferService) PostPendingTransfer(_ context.Context, req *transferpb.PostPendingTransferRequest) (*transferpb.PostPendingTransferResponse, error) {
	if f.postPendingTransfer == nil {
		panic("scriptedTransferService: PostPendingTransfer not scripted")
	}
	return f.postPendingTransfer(req)
}

func accepted(id string) *transferpb.RequestTransferResponse {
	return &transferpb.RequestTransferResponse{
		Id: id,
		Result: &transferpb.RequestTransferResponse_TransferRequestAccepted{
			TransferRequestAccepted: &transferpb.TransferRequestAccepted{Id: id},
		},
	}
}

func TestDriveOneTransferCommitsOnPlannedComplete(t *testing.T) {
	t.Parallel()

	transfers := &scriptedTransferService{
		requestTransfer: func(req *transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error) {
			return accepted(req.GetId()), nil
		},
		confirmStagedTransfer: func(req *transferpb.ConfirmStagedTransferRequest) (*transferpb.ConfirmStagedTransferResponse, error) {
			return &transferpb.ConfirmStagedTransferResponse{
				Id:     req.GetId(),
				Result: &transferpb.ConfirmStagedTransferResponse_TransferPending{TransferPending: &transferpb.TransferPending{Id: req.GetId()}},
			}, nil
		},
		postPendingTransfer: func(req *transferpb.PostPendingTransferRequest) (*transferpb.PostPendingTransferResponse, error) {
			return &transferpb.PostPendingTransferResponse{
				Id:     req.GetId(),
				Result: &transferpb.PostPendingTransferResponse_TransferCommitted{TransferCommitted: &transferpb.TransferCommitted{Id: req.GetId()}},
			}, nil
		},
	}

	result := driveOneTransfer(context.Background(), transfers, entity{walletID: "from"}, entity{walletID: "to"}, 500, "USD", outcomeComplete)

	if result.err != nil {
		t.Fatalf("driveOneTransfer: unexpected error: %v", result.err)
	}
	if !result.completed() {
		t.Errorf("result.completed() = false, want true (final=%s)", result.final)
	}
	if result.final != "TRANSFER_COMMITTED" {
		t.Errorf("final = %q, want TRANSFER_COMMITTED", result.final)
	}
}

func TestDriveOneTransferCancelsOnPlannedRollback(t *testing.T) {
	t.Parallel()

	transfers := &scriptedTransferService{
		requestTransfer: func(req *transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error) {
			return accepted(req.GetId()), nil
		},
		cancelStagedTransfer: func(req *transferpb.CancelStagedTransferRequest) (*transferpb.CancelStagedTransferResponse, error) {
			return &transferpb.CancelStagedTransferResponse{
				Id:     req.GetId(),
				Result: &transferpb.CancelStagedTransferResponse_TransferCancelled{TransferCancelled: &transferpb.TransferCancelled{Id: req.GetId(), Reason: req.GetReason()}},
			}, nil
		},
	}

	result := driveOneTransfer(context.Background(), transfers, entity{walletID: "from"}, entity{walletID: "to"}, 500, "USD", outcomeRollback)

	if result.err != nil {
		t.Fatalf("driveOneTransfer: unexpected error: %v", result.err)
	}
	if result.completed() {
		t.Error("result.completed() = true, want false — a rollback never moves money")
	}
	if result.final != "TRANSFER_CANCELLED" {
		t.Errorf("final = %q, want TRANSFER_CANCELLED", result.final)
	}
}

func TestDriveOneTransferReportsRequestTimeRejection(t *testing.T) {
	t.Parallel()

	transfers := &scriptedTransferService{
		requestTransfer: func(req *transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error) {
			return &transferpb.RequestTransferResponse{
				Id: req.GetId(),
				Result: &transferpb.RequestTransferResponse_TransferRequestRejected{
					TransferRequestRejected: &transferpb.TransferRequestRejected{Id: req.GetId(), Reason: "insufficient funds"},
				},
			}, nil
		},
	}

	result := driveOneTransfer(context.Background(), transfers, entity{walletID: "from"}, entity{walletID: "to"}, 500, "USD", outcomeComplete)

	if result.err != nil {
		t.Fatalf("driveOneTransfer: unexpected error: %v", result.err)
	}
	if result.completed() || result.stuck() {
		t.Errorf("result = %+v, want neither completed nor stuck", result)
	}
	if result.final != "TRANSFER_REQUEST_REJECTED" {
		t.Errorf("final = %q, want TRANSFER_REQUEST_REJECTED", result.final)
	}
	if result.reason != "insufficient funds" {
		t.Errorf("reason = %q, want %q", result.reason, "insufficient funds")
	}
}

func TestSettleTransferReportsStillOpenWhenNotYetStaged(t *testing.T) {
	t.Parallel()

	transfers := &scriptedTransferService{
		confirmStagedTransfer: func(req *transferpb.ConfirmStagedTransferRequest) (*transferpb.ConfirmStagedTransferResponse, error) {
			return &transferpb.ConfirmStagedTransferResponse{
				Id: req.GetId(),
				Result: &transferpb.ConfirmStagedTransferResponse_ConfirmStagedTransferRejected{
					ConfirmStagedTransferRejected: &transferpb.ConfirmStagedTransferRejected{Id: req.GetId(), Reason: "transfer is accepted, not staged"},
				},
			}, nil
		},
	}

	final, moved, open, reason, err := settleTransfer(context.Background(), transfers, "transfer1", outcomeComplete)

	if err != nil {
		t.Fatalf("settleTransfer: unexpected error: %v", err)
	}
	if moved {
		t.Error("moved = true, want false — nothing settled yet")
	}
	if !open {
		t.Error("open = false, want true — still waiting on the underlying Transfer to reach Staged")
	}
	if final != "TRANSFER_ACCEPTED" {
		t.Errorf("final = %q, want TRANSFER_ACCEPTED", final)
	}
	if reason != "transfer is accepted, not staged" {
		t.Errorf("reason = %q, want the rejection's own reason", reason)
	}
}
