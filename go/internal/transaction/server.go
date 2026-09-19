// Package transaction implements the Transaction root aggregate: a set of
// Transfers wired into a dependency DAG to accomplish one task (e.g. an ACH
// deposit's real custody leg plus its parallel shadow-tracking leg). The
// saga is synchronous and in-process, the same shape as transfer's own
// runSaga — see saga.go.
package transaction

import (
	"context"
	"errors"
	"fmt"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/id"
)

// AggregateType is this aggregate's stream namespace in the event log.
const AggregateType = "transaction"

// maxConcurrencyAttempts bounds every "load, decide, try to append" loop
// below, mirroring transfer.Server's own constant and reasoning: each
// attempt reloads current state, so a conflict never blindly retries the
// same decision.
const maxConcurrencyAttempts = 3

func abortedRetry(transactionID string) error {
	return twirp.NewError(twirp.Aborted, fmt.Sprintf(
		"transaction %q: could not converge after repeated concurrency conflicts; retry", transactionID,
	))
}

// transferClient is a narrow interface over exactly the transfer.Server
// methods the saga drives, called in-process as plain Go — never over
// Twirp/HTTP, the same reason transfer/saga.go calls
// operation.Stage/Perform/Cancel/Fail directly instead of through
// operation.Server's RPC surface. ConfirmStagedTransfer/PostPendingTransfer
// are deliberately absent: ruby calls those directly on a staged child's own
// id as part of the ResumeTransaction contract, never through Transaction.
type transferClient interface {
	RequestTransfer(ctx context.Context, req *transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error)
	CancelAcceptedTransfer(ctx context.Context, req *transferpb.CancelAcceptedTransferRequest) (*transferpb.CancelAcceptedTransferResponse, error)
	RequestReversal(ctx context.Context, req *transferpb.RequestReversalRequest) (*transferpb.RequestReversalResponse, error)
	CancelStagedTransfer(ctx context.Context, req *transferpb.CancelStagedTransferRequest) (*transferpb.CancelStagedTransferResponse, error)
	// WouldAcceptTransfer pre-flight-checks a non-mint_source child before
	// TransactionInitialized is ever written — see StartInitializingTransaction
	// and wouldAcceptReadyChildren.
	WouldAcceptTransfer(ctx context.Context, walletID string, amount *sharedpb.Money, callingTransactionID string) (*transferpb.TransferRequestRejected, error)
}

var _ pb.TransactionService = (*Server)(nil)

// Server implements the Transaction Twirp service.
type Server struct {
	store    eventstore.Store
	transfer transferClient
}

func NewServer(store eventstore.Store, transfer transferClient) *Server {
	return &Server{store: store, transfer: transfer}
}

// IsOpen implements wallet.TransactionOpenChecker: reports whether
// transactionID is still open — has not yet reached a terminal state, and
// so may still need to reverse a Token it tagged. Initialized, Started, and
// RollbackStarted are all open; so is RollbackFailed, deliberately — its
// stuck Tokens must stay protected while an operator manually reconciles
// it. Completed and RolledBack are closed. Rejected is moot (a rejected
// Transaction never reaches Started, so no child Transfer and therefore no
// tagged Token was ever created under it) but is treated as closed for
// completeness.
//
// Defensive default: a transaction_id whose stream doesn't exist at all —
// which should never happen, since a tag is only ever written from this
// Transaction's own dispatchReady, referencing itself — is treated as
// open: fail toward blocking, not toward risking a double-spend, since
// reaching this at all means an internal invariant was already violated.
func IsOpen(ctx context.Context, store eventstore.Store, transactionID string) (bool, error) {
	events, err := store.Load(ctx, AggregateType, transactionID)
	if err != nil {
		return false, twirp.InternalErrorWith(err)
	}
	switch topLevelState(events) {
	case stateCompleted, stateRolledBack, stateRejected:
		return false, nil
	default:
		return true, nil
	}
}

// Exists implements transfer.TransactionExistsChecker: reports whether
// transactionID names a real, already-initialized Transaction. Used only to
// authorize mint_source=true requests (see the Transfer plan's decision
// #10) — unlike IsOpen, a not-found stream here means reject, not "still
// open": a claimed transaction_id that doesn't resolve to a real stream is
// exactly what an attempt to bypass Transaction's authority looks like.
func Exists(ctx context.Context, store eventstore.Store, transactionID string) (bool, error) {
	events, err := store.Load(ctx, AggregateType, transactionID)
	if err != nil {
		return false, twirp.InternalErrorWith(err)
	}
	return len(events) > 0, nil
}

// StartInitializingTransaction accepts or rejects req, in both cases
// recording that decision as transactionID's first event — a rejection is
// as much a fact about this id's history as an acceptance is, the same
// reasoning transfer.RequestTransfer already uses. Two checks happen before
// TransactionInitialized is ever written, either of which produces
// TransactionRejected instead: DAG validation, and — new, see
// wouldAcceptReadyChildren and go/docs/adr/0004 — a pre-flight check of
// every non-mint_source child that would be dispatched immediately. The
// latter is a fast, best-effort check; the real dispatch inside runSaga
// below is unchanged and remains the sole authority regardless of what this
// pre-check found. Idempotent: a Transaction that was already decided —
// initialized or rejected — has its recorded outcome returned as-is.
func (s *Server) StartInitializingTransaction(ctx context.Context, req *pb.StartInitializingTransactionRequest) (*pb.StartInitializingTransactionResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxConcurrencyAttempts; attempt++ {
		events, err := s.store.Load(ctx, AggregateType, req.GetId())
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		if len(events) > 0 {
			return s.decidedStartInitializingTransaction(ctx, req.GetId(), events)
		}

		var event proto.Message
		switch dagErr := validateDAG(req.GetTransfers(), req.GetTransferDependency()); {
		case dagErr != nil:
			event = &pb.TransactionRejected{Id: req.GetId(), Reason: dagErr.Error()}
		default:
			reason, err := s.wouldAcceptReadyChildren(ctx, req.GetId(), req.GetTransfers(), req.GetTransferDependency())
			if err != nil {
				return nil, twirp.InternalErrorWith(err)
			}
			if reason != "" {
				event = &pb.TransactionRejected{Id: req.GetId(), Reason: reason}
			} else {
				event = &pb.TransactionInitialized{
					Id: req.GetId(), FactoryName: req.GetFactoryName(), FactoryVersion: req.GetFactoryVersion(),
					Transfers: req.GetTransfers(), TransferDependency: req.GetTransferDependency(),
				}
			}
		}

		switch err := s.store.Append(ctx, AggregateType, req.GetId(), 0, event); {
		case err == nil:
			if initialized, ok := event.(*pb.TransactionInitialized); ok {
				logSagaError("StartInitializingTransaction", req.GetId(), s.runSaga(ctx, req.GetId()))
				return initializedResponse(initialized), nil
			}
			return rejectedResponse(event.(*pb.TransactionRejected)), nil
		case errors.Is(err, eventstore.ErrConcurrencyConflict):
			continue // a concurrent write landed first — reload and re-decide
		default:
			return nil, twirp.InternalErrorWith(err)
		}
	}
	return nil, abortedRetry(req.GetId())
}

func (s *Server) decidedStartInitializingTransaction(ctx context.Context, transactionID string, events []eventstore.Event) (*pb.StartInitializingTransactionResponse, error) {
	msg, err := events[0].Decode()
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	switch m := msg.(type) {
	case *pb.TransactionInitialized:
		logSagaError("StartInitializingTransaction", transactionID, s.runSaga(ctx, transactionID))
		return initializedResponse(m), nil
	case *pb.TransactionRejected:
		return rejectedResponse(m), nil
	default:
		return nil, twirp.InternalError(fmt.Sprintf(
			"transaction %q: stream starts with %s, want TransactionInitialized or TransactionRejected", transactionID, events[0].EventType,
		))
	}
}

func initializedResponse(e *pb.TransactionInitialized) *pb.StartInitializingTransactionResponse {
	return &pb.StartInitializingTransactionResponse{
		Id: e.GetId(), Result: &pb.StartInitializingTransactionResponse_TransactionInitialized{TransactionInitialized: e},
	}
}

func rejectedResponse(e *pb.TransactionRejected) *pb.StartInitializingTransactionResponse {
	return &pb.StartInitializingTransactionResponse{
		Id: e.GetId(), Result: &pb.StartInitializingTransactionResponse_TransactionRejected{TransactionRejected: e},
	}
}

// StartProcessingTransfer triggers exactly one currently-gated child
// (auto_process=false, otherwise ready per its dependencies) despite its
// own auto_process flag saying to wait. Rejects — transiently; unlike a
// Transfer's own domain rejections, this isn't recorded onto the stream,
// since it reflects a caller-timing mistake rather than a durable business
// fact — when transfer_id isn't in this Transaction's DAG at all, or isn't
// currently Gated (not found, dependencies unsatisfied, or already
// processed).
func (s *Server) StartProcessingTransfer(ctx context.Context, req *pb.StartProcessingTransferRequest) (*pb.StartProcessingTransferResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}
	if err := id.Validate("transfer_id", req.GetTransferId()); err != nil {
		return nil, err
	}

	events, err := s.store.Load(ctx, AggregateType, req.GetId())
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return rejectedProcessing(req, "transaction not found"), nil
	}

	transfers, _, err := decodeSpec(events)
	if err != nil {
		return nil, err
	}
	spec, ok := transfers[req.GetTransferId()]
	if !ok {
		return rejectedProcessing(req, "transfer not found in this transaction's DAG"), nil
	}

	children, err := foldChildStates(events)
	if err != nil {
		return nil, err
	}
	if children[req.GetTransferId()] != childGated {
		return rejectedProcessing(req, fmt.Sprintf(
			"transfer %q is not gated (dependencies not yet satisfied, or already processed)", req.GetTransferId(),
		)), nil
	}

	if err := s.requestChildTransfer(ctx, req.GetId(), spec); err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	logSagaError("StartProcessingTransfer", req.GetId(), s.runSaga(ctx, req.GetId()))

	events, err = s.store.Load(ctx, AggregateType, req.GetId())
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	children, err = foldChildStates(events)
	if err != nil {
		return nil, err
	}
	switch children[req.GetTransferId()] {
	case childFailed:
		reason, err := failureReason(events, req.GetTransferId())
		if err != nil {
			return nil, err
		}
		return &pb.StartProcessingTransferResponse{
			Id: req.GetId(),
			Result: &pb.StartProcessingTransferResponse_TransferFailedWithinTransaction{
				TransferFailedWithinTransaction: &pb.TransferFailedWithinTransaction{Id: req.GetId(), TransferId: req.GetTransferId(), Reason: reason},
			},
		}, nil
	default:
		// childRequested or childCompleted (prepare+commit can resolve
		// synchronously within the same runSaga call above).
		return &pb.StartProcessingTransferResponse{
			Id: req.GetId(),
			Result: &pb.StartProcessingTransferResponse_TransferRequestedWithinTransaction{
				TransferRequestedWithinTransaction: &pb.TransferRequestedWithinTransaction{Id: req.GetId(), TransferId: req.GetTransferId()},
			},
		}, nil
	}
}

func rejectedProcessing(req *pb.StartProcessingTransferRequest, reason string) *pb.StartProcessingTransferResponse {
	return &pb.StartProcessingTransferResponse{
		Id: req.GetId(),
		Result: &pb.StartProcessingTransferResponse_StartProcessingTransferRejected{
			StartProcessingTransferRejected: &pb.StartProcessingTransferRejected{Id: req.GetId(), TransferId: req.GetTransferId(), Reason: reason},
		},
	}
}

// ResumeTransaction re-runs runSaga for transactionID and reports its
// resulting top-level state. This is Transaction's answer to having no
// naturally-repeated entry point the way every Transfer RPC re-runs
// transfer.runSaga on call: a staged child settles via calls made on ITS
// OWN id, which this Transaction has no way to learn about except by being
// asked to look again.
func (s *Server) ResumeTransaction(ctx context.Context, req *pb.ResumeTransactionRequest) (*pb.ResumeTransactionResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}

	events, err := s.store.Load(ctx, AggregateType, req.GetId())
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return nil, twirp.NewError(twirp.NotFound, fmt.Sprintf("transaction %q not found", req.GetId()))
	}

	logSagaError("ResumeTransaction", req.GetId(), s.runSaga(ctx, req.GetId()))

	events, err = s.store.Load(ctx, AggregateType, req.GetId())
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	reason, err := lastEventReason(events)
	if err != nil {
		return nil, err
	}
	return &pb.ResumeTransactionResponse{Id: req.GetId(), State: topLevelState(events).proto(), Reason: reason}, nil
}

// StartTransactionRollback is legal only while the Transaction is Started —
// mirroring CancelAcceptedTransfer's restriction to pre-commitment states.
// This is the manual counterpart to the automatic rollback runSaga already
// triggers on any child failure: an operator-driven abort of a Transaction
// that hasn't failed on its own. Idempotent once rollback has already begun
// or resolved; any other state is refused.
func (s *Server) StartTransactionRollback(ctx context.Context, req *pb.StartTransactionRollbackRequest) (*pb.StartTransactionRollbackResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}

	events, err := s.store.Load(ctx, AggregateType, req.GetId())
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return nil, twirp.NewError(twirp.NotFound, fmt.Sprintf("transaction %q not found", req.GetId()))
	}

	switch topLevelState(events) {
	case stateStarted:
		if err := s.appendSagaStep(ctx, req.GetId(), &pb.TransactionRollbackStarted{Id: req.GetId(), Reason: req.GetReason()}); err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		logSagaError("StartTransactionRollback", req.GetId(), s.runSaga(ctx, req.GetId()))
	case stateRollbackStarted, stateRolledBack, stateRollbackFailed:
		// Already on or past this path — idempotent no-op.
	default:
		return nil, twirp.NewError(twirp.FailedPrecondition, fmt.Sprintf("transaction %q cannot be rolled back from its current state", req.GetId()))
	}

	events, err = s.store.Load(ctx, AggregateType, req.GetId())
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	return &pb.StartTransactionRollbackResponse{Id: req.GetId(), State: topLevelState(events).proto()}, nil
}
