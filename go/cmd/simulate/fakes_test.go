package main

import (
	"context"

	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// These fakes script what the tool sees, one look at a time, so a test can
// say exactly what the orchestrator had done by each look. A method a fake
// does not define falls through to its embedded nil interface and panics: a
// test reaching one has drifted from what it scripted.

// The tool mints its own ids, so a script cannot name them. Each one is a
// sequence every id walks through on its own: the first look at an id gets
// the first entry, and the last entry repeats once the script runs out.

// scriptedLeg answers legReader from its script.
type scriptedLeg struct {
	outcomes []transfer.OutcomeKind
	looks    map[string]int
}

func newScriptedLeg(outcomes ...transfer.OutcomeKind) *scriptedLeg {
	return &scriptedLeg{outcomes: outcomes, looks: map[string]int{}}
}

func (l *scriptedLeg) read(_ context.Context, transferID string) (transfer.OutcomeKind, error) {
	i := min(l.looks[transferID], len(l.outcomes)-1)
	l.looks[transferID]++
	return l.outcomes[i], nil
}

// last is the outcome the tool most recently saw for transferID — what it
// believed about the leg when it made whatever call came next.
func (l *scriptedLeg) last(transferID string) transfer.OutcomeKind {
	n := l.looks[transferID]
	if n == 0 {
		return transfer.OutcomeNotFound
	}
	return l.outcomes[min(n, len(l.outcomes))-1]
}

// totalLooks is how many times the tool read any leg at all.
func (l *scriptedLeg) totalLooks() int {
	total := 0
	for _, n := range l.looks {
		total += n
	}
	return total
}

// recordingTransfers accepts every request and settlement, recording each
// settlement call with what the tool had last seen of the leg when it made it.
type recordingTransfers struct {
	transferpb.TransferService

	leg      *scriptedLeg
	rejectAs string // RequestTransfer rejects with this reason when set
	calls    []string
}

func (f *recordingTransfers) record(call, transferID string) {
	f.calls = append(f.calls, call+" at "+f.leg.last(transferID).String())
}

func (f *recordingTransfers) RequestTransfer(
	_ context.Context, req *transferpb.RequestTransferRequest,
) (*transferpb.RequestTransferResponse, error) {
	if f.rejectAs != "" {
		return &transferpb.RequestTransferResponse{Id: req.GetId(), Result: &transferpb.RequestTransferResponse_TransferRequestRejected{
			TransferRequestRejected: &transferpb.TransferRequestRejected{Id: req.GetId(), Reason: f.rejectAs},
		}}, nil
	}
	return &transferpb.RequestTransferResponse{Id: req.GetId(), Result: &transferpb.RequestTransferResponse_TransferRequestAccepted{
		TransferRequestAccepted: &transferpb.TransferRequestAccepted{Id: req.GetId()},
	}}, nil
}

func (f *recordingTransfers) ConfirmStagedTransfer(
	_ context.Context, req *transferpb.ConfirmStagedTransferRequest,
) (*transferpb.ConfirmStagedTransferResponse, error) {
	f.record("confirm", req.GetId())
	return &transferpb.ConfirmStagedTransferResponse{Id: req.GetId(), Result: &transferpb.ConfirmStagedTransferResponse_TransferPending{
		TransferPending: &transferpb.TransferPending{Id: req.GetId()},
	}}, nil
}

func (f *recordingTransfers) PostPendingTransfer(
	_ context.Context, req *transferpb.PostPendingTransferRequest,
) (*transferpb.PostPendingTransferResponse, error) {
	f.record("post", req.GetId())
	return &transferpb.PostPendingTransferResponse{Id: req.GetId(), Result: &transferpb.PostPendingTransferResponse_TransferCommitted{
		TransferCommitted: &transferpb.TransferCommitted{Id: req.GetId()},
	}}, nil
}

func (f *recordingTransfers) CancelStagedTransfer(
	_ context.Context, req *transferpb.CancelStagedTransferRequest,
) (*transferpb.CancelStagedTransferResponse, error) {
	f.record("cancel", req.GetId())
	return &transferpb.CancelStagedTransferResponse{Id: req.GetId(), Result: &transferpb.CancelStagedTransferResponse_TransferCancelled{
		TransferCancelled: &transferpb.TransferCancelled{Id: req.GetId(), Reason: req.GetReason()},
	}}, nil
}

// scriptedTransactions answers GetTransactionState from its script, the same
// way scriptedLeg does, and accepts every start unless told to reject it.
// Rollback requests are recorded like recordingTransfers' settlements, against
// the leg the tool last saw.
type scriptedTransactions struct {
	transactionpb.TransactionService

	states   []transactionpb.TransactionState
	looks    map[string]int
	rejectAs string // StartInitializingTransaction rejects with this reason when set

	leg       *scriptedLeg
	legOf     map[string]string // transaction id -> its leg's transfer id
	rollbacks []string
}

func newScriptedTransactions(states ...transactionpb.TransactionState) *scriptedTransactions {
	return &scriptedTransactions{states: states, looks: map[string]int{}, legOf: map[string]string{}}
}

func (f *scriptedTransactions) StartInitializingTransaction(
	_ context.Context, req *transactionpb.StartInitializingTransactionRequest,
) (*transactionpb.StartInitializingTransactionResponse, error) {
	for transferID := range req.GetTransfers() {
		f.legOf[req.GetId()] = transferID
	}
	if f.rejectAs != "" {
		return &transactionpb.StartInitializingTransactionResponse{Id: req.GetId(), Result: &transactionpb.StartInitializingTransactionResponse_TransactionRejected{
			TransactionRejected: &transactionpb.TransactionRejected{Id: req.GetId(), Reason: f.rejectAs},
		}}, nil
	}
	return &transactionpb.StartInitializingTransactionResponse{Id: req.GetId(), Result: &transactionpb.StartInitializingTransactionResponse_TransactionInitialized{
		TransactionInitialized: &transactionpb.TransactionInitialized{Id: req.GetId()},
	}}, nil
}

func (f *scriptedTransactions) GetTransactionState(
	_ context.Context, req *transactionpb.GetTransactionStateRequest,
) (*transactionpb.GetTransactionStateResponse, error) {
	i := min(f.looks[req.GetId()], len(f.states)-1)
	f.looks[req.GetId()]++
	return &transactionpb.GetTransactionStateResponse{Id: req.GetId(), State: f.states[i]}, nil
}

func (f *scriptedTransactions) StartTransactionRollback(
	_ context.Context, req *transactionpb.StartTransactionRollbackRequest,
) (*transactionpb.StartTransactionRollbackResponse, error) {
	seen := transfer.OutcomeNotFound
	if f.leg != nil {
		seen = f.leg.last(f.legOf[req.GetId()])
	}
	f.rollbacks = append(f.rollbacks, "rollback at "+seen.String())
	return &transactionpb.StartTransactionRollbackResponse{
		Id: req.GetId(), State: transactionpb.TransactionState_TRANSACTION_STATE_ROLLBACK_STARTED,
	}, nil
}
