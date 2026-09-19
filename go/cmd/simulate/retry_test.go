package main

import (
	"context"
	"testing"
	"time"

	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
)

// fakeTransactions implements transactionpb.TransactionService, scripted per
// id: ResumeTransaction returns the states listed, repeating the last once
// exhausted. Only ResumeTransaction is exercised by settle's complete
// branch; every other method panics, since a test reaching one has drifted
// from what it means to script this fake.
type fakeTransactions struct {
	resumeStates map[string][]transactionpb.TransactionState
	calls        map[string]int
}

func (f *fakeTransactions) StartInitializingTransaction(context.Context, *transactionpb.StartInitializingTransactionRequest) (*transactionpb.StartInitializingTransactionResponse, error) {
	panic("fakeTransactions: StartInitializingTransaction not scripted")
}

func (f *fakeTransactions) StartProcessingTransfer(context.Context, *transactionpb.StartProcessingTransferRequest) (*transactionpb.StartProcessingTransferResponse, error) {
	panic("fakeTransactions: StartProcessingTransfer not scripted")
}

func (f *fakeTransactions) ResumeTransaction(_ context.Context, req *transactionpb.ResumeTransactionRequest) (*transactionpb.ResumeTransactionResponse, error) {
	states := f.resumeStates[req.GetId()]
	i := f.calls[req.GetId()]
	if i >= len(states) {
		i = len(states) - 1
	}
	f.calls[req.GetId()]++
	return &transactionpb.ResumeTransactionResponse{Id: req.GetId(), State: states[i]}, nil
}

func (f *fakeTransactions) StartTransactionRollback(context.Context, *transactionpb.StartTransactionRollbackRequest) (*transactionpb.StartTransactionRollbackResponse, error) {
	panic("fakeTransactions: StartTransactionRollback not scripted")
}

// fakeTransfers implements transferpb.TransferService, scripted per transfer
// id: ConfirmStagedTransfer rejects rejectRemaining[id] more times (as if
// the orchestrator hasn't yet resumed a Transfer stuck at Accepted) before
// accepting; PostPendingTransfer always accepts once reached.
type fakeTransfers struct {
	rejectRemaining map[string]int
}

func (f *fakeTransfers) RequestTransfer(context.Context, *transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error) {
	panic("fakeTransfers: RequestTransfer not scripted")
}

func (f *fakeTransfers) CancelAcceptedTransfer(context.Context, *transferpb.CancelAcceptedTransferRequest) (*transferpb.CancelAcceptedTransferResponse, error) {
	panic("fakeTransfers: CancelAcceptedTransfer not scripted")
}

func (f *fakeTransfers) RequestReversal(context.Context, *transferpb.RequestReversalRequest) (*transferpb.RequestReversalResponse, error) {
	panic("fakeTransfers: RequestReversal not scripted")
}

func (f *fakeTransfers) ConfirmStagedTransfer(_ context.Context, req *transferpb.ConfirmStagedTransferRequest) (*transferpb.ConfirmStagedTransferResponse, error) {
	if f.rejectRemaining[req.GetId()] > 0 {
		f.rejectRemaining[req.GetId()]--
		return &transferpb.ConfirmStagedTransferResponse{
			Id: req.GetId(),
			Result: &transferpb.ConfirmStagedTransferResponse_ConfirmStagedTransferRejected{
				ConfirmStagedTransferRejected: &transferpb.ConfirmStagedTransferRejected{Id: req.GetId(), Reason: "transfer is accepted, not staged"},
			},
		}, nil
	}
	return &transferpb.ConfirmStagedTransferResponse{
		Id:     req.GetId(),
		Result: &transferpb.ConfirmStagedTransferResponse_TransferPending{TransferPending: &transferpb.TransferPending{Id: req.GetId()}},
	}, nil
}

func (f *fakeTransfers) CancelStagedTransfer(context.Context, *transferpb.CancelStagedTransferRequest) (*transferpb.CancelStagedTransferResponse, error) {
	panic("fakeTransfers: CancelStagedTransfer not scripted")
}

func (f *fakeTransfers) PostPendingTransfer(_ context.Context, req *transferpb.PostPendingTransferRequest) (*transferpb.PostPendingTransferResponse, error) {
	return &transferpb.PostPendingTransferResponse{
		Id:     req.GetId(),
		Result: &transferpb.PostPendingTransferResponse_TransferCommitted{TransferCommitted: &transferpb.TransferCommitted{Id: req.GetId()}},
	}, nil
}

// transactionRetry builds the same retry closure driverFor gives
// -mode=transaction, over the fakes these tests script.
func transactionRetry(transactions transactionpb.TransactionService, transfers transferpb.TransferService) retryFunc {
	return func(ctx context.Context, r *txResult) {
		state, reason, err := settle(ctx, transactions, transfers, r.transactionID, r.transferID, r.planned)
		r.final, r.moved, r.open = fromTransactionState(state)
		r.reason, r.err = reason, err
	}
}

func TestRetryStuckConvergesOnceTheUnderlyingTransferCatchesUp(t *testing.T) {
	t.Parallel()

	transactions := &fakeTransactions{
		resumeStates: map[string][]transactionpb.TransactionState{
			"tx1": {transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED},
		},
		calls: map[string]int{},
	}
	// Rejects on the first retry attempt (as if the orchestrator hadn't
	// caught up yet), accepts on the second.
	transfers := &fakeTransfers{rejectRemaining: map[string]int{"transfer1": 1}}

	results := []txResult{
		{transactionID: "tx1", transferID: "transfer1", planned: outcomeComplete, final: "TRANSACTION_STATE_STARTED", open: true},
	}

	remaining := retryStuck(context.Background(), results, 3, time.Millisecond, transactionRetry(transactions, transfers))

	if remaining != 0 {
		t.Errorf("retryStuck: remaining = %d, want 0", remaining)
	}
	if got := results[0].final; got != "TRANSACTION_STATE_COMPLETED" {
		t.Errorf("results[0].final = %s, want COMPLETED", got)
	}
	if results[0].err != nil {
		t.Errorf("results[0].err = %v, want nil", results[0].err)
	}
}

func TestRetryStuckGivesUpAfterExhaustingAttempts(t *testing.T) {
	t.Parallel()

	transactions := &fakeTransactions{calls: map[string]int{}}
	transfers := &fakeTransfers{rejectRemaining: map[string]int{"transfer1": 100}} // never catches up

	results := []txResult{
		{transactionID: "tx1", transferID: "transfer1", planned: outcomeComplete, final: "TRANSACTION_STATE_STARTED", open: true},
	}

	remaining := retryStuck(context.Background(), results, 2, time.Millisecond, transactionRetry(transactions, transfers))

	if remaining != 1 {
		t.Errorf("retryStuck: remaining = %d, want 1", remaining)
	}
	if got := results[0].final; got != "TRANSACTION_STATE_STARTED" {
		t.Errorf("results[0].final = %s, want still STARTED", got)
	}
}

func TestRetryStuckSkipsResultsThatAreNotStuck(t *testing.T) {
	t.Parallel()

	transactions := &fakeTransactions{calls: map[string]int{}}
	transfers := &fakeTransfers{}

	results := []txResult{
		{transactionID: "tx1", final: "TRANSACTION_STATE_COMPLETED", moved: true},
		{transactionID: "tx2", final: "TRANSACTION_STATE_ROLLED_BACK"},
	}

	remaining := retryStuck(context.Background(), results, 3, time.Millisecond, transactionRetry(transactions, transfers))

	if remaining != 0 {
		t.Errorf("retryStuck: remaining = %d, want 0 (nothing was stuck)", remaining)
	}
}

func TestStuckIndices(t *testing.T) {
	t.Parallel()

	results := []txResult{
		{final: "TRANSACTION_STATE_COMPLETED", moved: true},
		{final: "TRANSACTION_STATE_STARTED", open: true},
		{final: "TRANSACTION_STATE_ROLLED_BACK"},
		{final: "TRANSACTION_STATE_STARTED", open: true},
	}

	got := stuckIndices(results)
	want := []int{1, 3}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("stuckIndices = %v, want %v", got, want)
	}
}
