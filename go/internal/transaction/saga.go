package transaction

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/detid"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// transactionState is this Transaction's own top-level progression, folded
// from its own stream — the same "one state per real transition" idiom
// transfer.currentState uses.
type transactionState int

const (
	stateUnknown transactionState = iota
	stateInitialized
	stateStarted
	stateRollbackStarted
	stateCompleted
	stateRolledBack
	stateRollbackFailed
	stateRejected
)

func (s transactionState) String() string {
	switch s {
	case stateInitialized:
		return "initialized"
	case stateStarted:
		return "started"
	case stateRollbackStarted:
		return "rollback_started"
	case stateCompleted:
		return "completed"
	case stateRolledBack:
		return "rolled_back"
	case stateRollbackFailed:
		return "rollback_failed"
	case stateRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

func (s transactionState) proto() pb.TransactionState {
	switch s {
	case stateInitialized:
		return pb.TransactionState_TRANSACTION_STATE_INITIALIZED
	case stateStarted:
		return pb.TransactionState_TRANSACTION_STATE_STARTED
	case stateRollbackStarted:
		return pb.TransactionState_TRANSACTION_STATE_ROLLBACK_STARTED
	case stateCompleted:
		return pb.TransactionState_TRANSACTION_STATE_COMPLETED
	case stateRolledBack:
		return pb.TransactionState_TRANSACTION_STATE_ROLLED_BACK
	case stateRollbackFailed:
		return pb.TransactionState_TRANSACTION_STATE_ROLLBACK_FAILED
	case stateRejected:
		return pb.TransactionState_TRANSACTION_STATE_REJECTED
	default:
		return pb.TransactionState_TRANSACTION_STATE_UNSPECIFIED
	}
}

// topLevelState folds a Transaction's stream to find which top-level state
// it's currently in, based on the last top-level-transition event recorded.
func topLevelState(events []eventstore.Event) transactionState {
	state := stateUnknown
	for _, e := range events {
		switch e.EventType {
		case eventstore.EventType(&pb.TransactionInitialized{}):
			state = stateInitialized
		case eventstore.EventType(&pb.TransactionRejected{}):
			state = stateRejected
		case eventstore.EventType(&pb.TransactionStarted{}):
			state = stateStarted
		case eventstore.EventType(&pb.TransactionRollbackStarted{}):
			state = stateRollbackStarted
		case eventstore.EventType(&pb.TransactionCompleted{}):
			state = stateCompleted
		case eventstore.EventType(&pb.TransactionRolledBack{}):
			state = stateRolledBack
		case eventstore.EventType(&pb.TransactionRollbackFailed{}):
			state = stateRollbackFailed
		}
	}
	return state
}

// lastEventReason reads the reason off whichever reason-carrying event
// (TransactionRejected/RollbackStarted/RolledBack/RollbackFailed) most
// recently landed — used to answer ResumeTransaction/StartTransactionRollback
// callers without making them separately walk the log.
func lastEventReason(events []eventstore.Event) (string, error) {
	if len(events) == 0 {
		return "", nil
	}
	msg, err := events[len(events)-1].Decode()
	if err != nil {
		return "", twirp.InternalErrorWith(err)
	}
	switch m := msg.(type) {
	case *pb.TransactionRejected:
		return m.GetReason(), nil
	case *pb.TransactionRollbackStarted:
		return m.GetReason(), nil
	case *pb.TransactionRolledBack:
		return m.GetReason(), nil
	case *pb.TransactionRollbackFailed:
		return m.GetReason(), nil
	default:
		return "", nil
	}
}

// rollbackStartedReason recovers the reason rollback began for, so the
// eventual TransactionRolledBack/TransactionRollbackFailed can carry it
// forward instead of landing with an empty reason.
func rollbackStartedReason(events []eventstore.Event) (string, error) {
	for _, e := range events {
		if e.EventType != eventstore.EventType(&pb.TransactionRollbackStarted{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			return "", twirp.InternalErrorWith(err)
		}
		started, ok := msg.(*pb.TransactionRollbackStarted)
		if !ok {
			return "", nil
		}
		return started.GetReason(), nil
	}
	return "", nil
}

// childState is one child Transfer's progression within the Transaction,
// folded from the Transaction's own per-child bookkeeping events.
type childState int

const (
	childUntouched childState = iota
	childGated
	childRequested
	childCompleted
	childFailed
	// childRollbackRequested is the rollback-side twin of childRequested: a
	// Reversal has been requested for this child but has not yet reached a
	// terminal state of its own. A Reversal is a whole Transfer running its own
	// saga, so "undo this child" and "this child is undone" are two facts that
	// can be separated in time — see go/docs/adr/0002.
	childRollbackRequested
	childRolledBack
	childRollbackFailed
)

// foldChildStates walks every event once, keyed by transfer_id, last-event-
// wins per id — the same "last relevant saga-outcome event wins" idiom
// transfer.currentState uses for a single Transfer, just keyed by a map
// instead of a scalar. A child with no entry has never been touched.
func foldChildStates(events []eventstore.Event) (map[string]childState, error) {
	states := make(map[string]childState)
	for _, e := range events {
		msg, err := e.Decode()
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		switch m := msg.(type) {
		case *pb.TransferGatedWithinTransaction:
			states[m.GetTransferId()] = childGated
		case *pb.TransferRequestedWithinTransaction:
			states[m.GetTransferId()] = childRequested
		case *pb.TransferCompletedWithinTransaction:
			states[m.GetTransferId()] = childCompleted
		case *pb.TransferFailedWithinTransaction:
			states[m.GetTransferId()] = childFailed
		case *pb.TransferReversalRequestedWithinTransaction:
			states[m.GetTransferId()] = childRollbackRequested
		case *pb.TransferRolledBackWithinTransaction:
			states[m.GetTransferId()] = childRolledBack
		case *pb.TransferRollbackFailedWithinTransaction:
			states[m.GetTransferId()] = childRollbackFailed
		}
	}
	return states, nil
}

func touchedSet(children map[string]childState) map[string]bool {
	touched := make(map[string]bool, len(children))
	for id := range children {
		touched[id] = true
	}
	return touched
}

func completedSet(children map[string]childState) map[string]bool {
	completed := make(map[string]bool, len(children))
	for id, s := range children {
		if s == childCompleted {
			completed[id] = true
		}
	}
	return completed
}

// firstFailed returns some child (iteration order doesn't matter: any
// failure triggers the same whole-transaction rollback, per the confirmed
// all-or-nothing decision) currently in a failed state, if any.
func firstFailed(children map[string]childState) (transferID string, ok bool) {
	for id, s := range children {
		if s == childFailed {
			return id, true
		}
	}
	return "", false
}

// failureReason scans events for the reason a specific child's
// TransferFailedWithinTransaction recorded.
func failureReason(events []eventstore.Event, transferID string) (string, error) {
	for _, e := range events {
		if e.EventType != eventstore.EventType(&pb.TransferFailedWithinTransaction{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			return "", twirp.InternalErrorWith(err)
		}
		failed, ok := msg.(*pb.TransferFailedWithinTransaction)
		if ok && failed.GetTransferId() == transferID {
			return failed.GetReason(), nil
		}
	}
	return "", nil
}

// allTerminal reports whether every child in the DAG has completed. Only
// ever checked once nothing failed and nothing is dispatchable — a
// leftover Gated child at that point just means the Transaction is
// legitimately still waiting on an external StartProcessingTransfer, not
// that it's stuck.
func allTerminal(transfers map[string]*pb.Transfer, children map[string]childState) bool {
	for id := range transfers {
		if children[id] != childCompleted {
			return false
		}
	}
	return true
}

// childEventTransferID extracts the transfer_id field from any of the
// per-child bookkeeping events, or "" for top-level events
// (TransactionStarted, TransactionCompleted, and so on) that carry no such
// field — those are naturally singleton-per-stream anyway (a Transaction
// only ever starts once, completes once...), so sharing "" as their
// dedup key is exactly the right equality check for appendSagaStep.
func childEventTransferID(msg proto.Message) string {
	switch m := msg.(type) {
	case *pb.TransferRequestedWithinTransaction:
		return m.GetTransferId()
	case *pb.TransferGatedWithinTransaction:
		return m.GetTransferId()
	case *pb.TransferCompletedWithinTransaction:
		return m.GetTransferId()
	case *pb.TransferFailedWithinTransaction:
		return m.GetTransferId()
	case *pb.TransferReversalRequestedWithinTransaction:
		return m.GetTransferId()
	case *pb.TransferRolledBackWithinTransaction:
		return m.GetTransferId()
	case *pb.TransferRollbackFailedWithinTransaction:
		return m.GetTransferId()
	default:
		return ""
	}
}

// appendSagaStep appends event as the next fact on transactionID's own
// stream, unless an event of the exact same type recording the same child
// (by transfer_id) is already there — idempotent convergence, mirroring
// transfer.Server's own appendSagaStep idiom. Unlike a single Transfer's
// own stream, a Transaction's stream interleaves many different children's
// events of the same handful of types, so checking only the stream's tail
// (as transfer's version does) isn't enough here — this scans by (type,
// transfer_id) pair instead.
func (s *Server) appendSagaStep(ctx context.Context, transactionID string, event proto.Message) error {
	events, err := s.store.Load(ctx, AggregateType, transactionID)
	if err != nil {
		return twirp.InternalErrorWith(err)
	}

	wantType := eventstore.EventType(event)
	wantChildID := childEventTransferID(event)
	for _, e := range events {
		if e.EventType != wantType {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			return twirp.InternalErrorWith(err)
		}
		if childEventTransferID(msg) == wantChildID {
			return nil // already recorded
		}
	}

	switch err := s.store.Append(ctx, AggregateType, transactionID, int64(len(events)), event); {
	case err == nil:
		return nil
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		return s.appendSagaStep(ctx, transactionID, event)
	default:
		return twirp.InternalErrorWith(err)
	}
}

// logSagaError reports a runSaga failure that was deliberately not surfaced
// to the RPC caller — the accept/reject or per-child decision that
// triggered this runSaga run is already durable by the time runSaga
// executes, the same reasoning transfer.Server's own logSagaError
// documents. The Transaction self-heals: it stays wherever runSaga left it,
// and the next call that touches this id resumes it.
func logSagaError(rpc, transactionID string, err error) {
	if err != nil {
		log.Printf("transaction: %s(%s): saga did not complete: %v", rpc, transactionID, err)
	}
}

// runSaga folds the Transaction's current state and dispatches the next
// step, looping until it reaches a state that waits on something outside
// this call — an in-flight or gated child, or an external
// StartProcessingTransfer/StartTransactionRollback — or a true terminal
// (Completed, RolledBack, RollbackFailed, Rejected). Safe, and expected, to
// call idempotently any number of times for the same id: each call resumes
// from wherever the stream actually left off.
func (s *Server) runSaga(ctx context.Context, transactionID string) error {
	for {
		events, err := s.store.Load(ctx, AggregateType, transactionID)
		if err != nil {
			return twirp.InternalErrorWith(err)
		}
		if len(events) == 0 {
			return fmt.Errorf("transaction %q: no events", transactionID)
		}

		switch top := topLevelState(events); top {
		case stateInitialized:
			if err := s.appendSagaStep(ctx, transactionID, &pb.TransactionStarted{Id: transactionID}); err != nil {
				return err
			}

		case stateStarted:
			transfers, deps, err := decodeSpec(events)
			if err != nil {
				return err
			}
			children, err := foldChildStates(events)
			if err != nil {
				return err
			}

			progressed, err := s.reconcileInFlight(ctx, transactionID, children)
			if err != nil {
				return err
			}
			if progressed {
				continue
			}

			if failedID, ok := firstFailed(children); ok {
				reason, err := failureReason(events, failedID)
				if err != nil {
					return err
				}
				if err := s.appendSagaStep(ctx, transactionID, &pb.TransactionRollbackStarted{
					Id: transactionID, Reason: fmt.Sprintf("child %q failed: %s", failedID, reason),
				}); err != nil {
					return err
				}
				continue
			}

			if allTerminal(transfers, children) {
				if err := s.appendSagaStep(ctx, transactionID, &pb.TransactionCompleted{Id: transactionID}); err != nil {
					return err
				}
				continue
			}

			dispatched, err := s.dispatchReady(ctx, transactionID, transfers, deps, children)
			if err != nil {
				return err
			}
			if dispatched {
				continue
			}
			return nil // waiting on an in-flight/gated child or an external StartProcessingTransfer

		case stateRollbackStarted:
			transfers, deps, err := decodeSpec(events)
			if err != nil {
				return err
			}
			children, err := foldChildStates(events)
			if err != nil {
				return err
			}

			reconciled, err := s.reconcileRollbacks(ctx, transactionID, events, children)
			if err != nil {
				return err
			}
			if reconciled {
				continue
			}

			progressed, waiting, blocked, err := s.rollbackNext(ctx, transactionID, transfers, deps, children)
			if err != nil {
				return err
			}
			switch {
			case progressed:
				continue
			case waiting:
				// At least one child is parked on a Reversal that has not
				// resolved. Nothing to decide yet, and nothing may be claimed:
				// stop here and resume when something asks again.
				return nil
			case blocked:
				reason, err := rollbackStartedReason(events)
				if err != nil {
					return err
				}
				if reason == "" {
					reason = "one or more children could not be rolled back; see TransferRollbackFailedWithinTransaction"
				}
				if err := s.appendSagaStep(ctx, transactionID, &pb.TransactionRollbackFailed{Id: transactionID, Reason: reason}); err != nil {
					return err
				}
				continue
			default:
				reason, err := rollbackStartedReason(events)
				if err != nil {
					return err
				}
				if err := s.appendSagaStep(ctx, transactionID, &pb.TransactionRolledBack{Id: transactionID, Reason: reason}); err != nil {
					return err
				}
				continue
			}

		case stateCompleted, stateRolledBack, stateRollbackFailed, stateRejected:
			return nil

		default:
			return fmt.Errorf("transaction %q: saga stuck in unrecognized state", transactionID)
		}
	}
}

// dispatchReady dispatches every currently-ready child (per readyToRun):
// auto_process=true children get an actual RequestTransfer call;
// auto_process=false children are simply marked Gated, waiting for an
// explicit StartProcessingTransfer. Returns true if it made any progress,
// so runSaga knows to loop again rather than stopping.
func (s *Server) dispatchReady(
	ctx context.Context, transactionID string, transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList, children map[string]childState,
) (bool, error) {
	ready := readyToRun(transfers, deps, touchedSet(children), completedSet(children))
	if len(ready) == 0 {
		return false, nil
	}

	for _, childID := range ready {
		spec := transfers[childID]
		if !spec.GetAutoProcess() {
			if err := s.appendSagaStep(ctx, transactionID, &pb.TransferGatedWithinTransaction{Id: transactionID, TransferId: childID}); err != nil {
				return false, err
			}
			continue
		}
		if err := s.requestChildTransfer(ctx, transactionID, spec); err != nil {
			return false, err
		}
	}
	return true, nil
}

// requestChildTransfer calls transfer.RequestTransfer for spec, tagged with
// transactionID, and records the outcome onto the Transaction's own
// stream: TransferRequestedWithinTransaction on accept, or
// TransferFailedWithinTransaction directly on an immediate accept-time
// rejection — exactly as much a "child failed" fact as a later TransferFailed
// reaching it through reconcileInFlight is.
func (s *Server) requestChildTransfer(ctx context.Context, transactionID string, spec *pb.Transfer) error {
	resp, err := s.transfer.RequestTransfer(ctx, &transferpb.RequestTransferRequest{
		Id: spec.GetId(), FromWalletId: spec.GetFromWalletId(), ToWalletId: spec.GetToWalletId(),
		Amount: spec.GetAmount(), Stage: spec.GetStage(), MintSource: spec.GetMintSource(),
		TransactionId: transactionID,
	})
	if err != nil {
		return err
	}
	if rejected := resp.GetTransferRequestRejected(); rejected != nil {
		return s.appendSagaStep(ctx, transactionID, &pb.TransferFailedWithinTransaction{
			Id: transactionID, TransferId: spec.GetId(), Reason: rejected.GetReason(),
		})
	}
	return s.appendSagaStep(ctx, transactionID, &pb.TransferRequestedWithinTransaction{Id: transactionID, TransferId: spec.GetId()})
}

// reconcileInFlight checks every currently-Requested child's live
// transfer.Outcome and records how it's resolved, if it has: Committed ->
// TransferCompletedWithinTransaction; Failed or Cancelled ->
// TransferFailedWithinTransaction. Anything else (InFlight, Staged,
// Pending) means still waiting on the outside world — no change, matching
// transfer.runSaga itself stopping at Staged/Pending. Returns true if it
// recorded anything, so runSaga knows to loop again.
func (s *Server) reconcileInFlight(ctx context.Context, transactionID string, children map[string]childState) (bool, error) {
	progressed := false
	for childID, state := range children {
		if state != childRequested {
			continue
		}
		outcome, err := transfer.Outcome(ctx, s.store, childID)
		if err != nil {
			return false, err
		}
		switch outcome {
		case transfer.OutcomeCommitted:
			if err := s.appendSagaStep(ctx, transactionID, &pb.TransferCompletedWithinTransaction{Id: transactionID, TransferId: childID}); err != nil {
				return false, err
			}
			progressed = true
		case transfer.OutcomeFailed, transfer.OutcomeCancelled:
			if err := s.appendSagaStep(ctx, transactionID, &pb.TransferFailedWithinTransaction{
				Id: transactionID, TransferId: childID, Reason: fmt.Sprintf("child transfer reached %s", outcome),
			}); err != nil {
				return false, err
			}
			progressed = true
		}
	}
	return progressed, nil
}

// requestedReversalID recovers which Reversal a child is waiting on, from the
// TransferReversalRequestedWithinTransaction that recorded it.
func requestedReversalID(events []eventstore.Event, transferID string) (string, error) {
	for _, e := range events {
		if e.EventType != eventstore.EventType(&pb.TransferReversalRequestedWithinTransaction{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			return "", twirp.InternalErrorWith(err)
		}
		if requested, ok := msg.(*pb.TransferReversalRequestedWithinTransaction); ok && requested.GetTransferId() == transferID {
			return requested.GetReversalId(), nil
		}
	}
	return "", nil
}

// reconcileRollbacks checks every child waiting on a Reversal and records how
// that Reversal resolved, if it has: Committed -> TransferRolledBackWithinTransaction
// (REVERSED); Failed or Cancelled -> TransferRollbackFailedWithinTransaction.
// Anything else means the Reversal is still running its own saga — including
// sitting Staged or Pending, since a Reversal inherits the original's staging
// requirement and so can wait on the outside world for as long as the original
// did. Returns true if it recorded anything, so runSaga knows to loop again.
// This is deliberately the same shape as reconcileInFlight: the forward and
// rollback paths now resolve their in-flight children the same way.
func (s *Server) reconcileRollbacks(
	ctx context.Context, transactionID string, events []eventstore.Event, children map[string]childState,
) (bool, error) {
	progressed := false
	for childID, state := range children {
		if state != childRollbackRequested {
			continue
		}
		reversalID, err := requestedReversalID(events, childID)
		if err != nil {
			return false, err
		}
		if reversalID == "" {
			return false, fmt.Errorf("transaction %q: child %q is awaiting a reversal with no recorded id", transactionID, childID)
		}
		outcome, err := transfer.Outcome(ctx, s.store, reversalID)
		if err != nil {
			return false, err
		}
		switch outcome {
		case transfer.OutcomeCommitted:
			if err := s.appendSagaStep(ctx, transactionID, &pb.TransferRolledBackWithinTransaction{
				Id: transactionID, TransferId: childID,
				Method: pb.RollbackMethod_ROLLBACK_METHOD_REVERSED, DetailId: reversalID,
			}); err != nil {
				return false, err
			}
			progressed = true
		case transfer.OutcomeFailed, transfer.OutcomeCancelled, transfer.OutcomeRejected:
			if err := s.appendSagaStep(ctx, transactionID, &pb.TransferRollbackFailedWithinTransaction{
				Id: transactionID, TransferId: childID,
				Reason: fmt.Sprintf("reversal %q reached %s instead of committing", reversalID, outcome),
			}); err != nil {
				return false, err
			}
			progressed = true
		}
	}
	return progressed, nil
}

// rollbackNext advances rollback by one sweep: every child readyToRollback
// gets rolled back per its LIVE transfer.Outcome (not whatever Transaction
// last recorded, since the outside world may have moved it since).
// Returns (progressed, waiting, blocked): progressed is true if this sweep
// recorded anything; waiting is true if at least one child is parked on a
// Reversal that has not resolved yet, which is the caller's signal to stop
// without claiming any terminal state; blocked is true if the sweep recorded
// nothing further AND at least one child has ever recorded a rollback failure —
// the caller's signal to land on TransactionRollbackFailed instead of
// TransactionRolledBack. A child already in childRollbackFailed is skipped
// on every subsequent sweep rather than retried automatically forever.
func (s *Server) rollbackNext(
	ctx context.Context, transactionID string, transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList, children map[string]childState,
) (progressed, waiting, blocked bool, err error) {
	rolledBack := make(map[string]bool, len(children))
	inFlight := make(map[string]bool, len(children))
	anyRollbackFailed := false
	for id, st := range children {
		switch st {
		case childRolledBack:
			rolledBack[id] = true
		case childRollbackRequested:
			inFlight[id] = true
		case childRollbackFailed:
			anyRollbackFailed = true
		}
	}

	ready := readyToRollback(transfers, deps, touchedSet(children), rolledBack, inFlight)

	madeProgress := false
	for _, childID := range ready {
		if children[childID] == childRollbackFailed {
			continue // already recorded as stuck; not retried automatically
		}
		if err := s.rollbackChild(ctx, transactionID, transfers[childID], children[childID]); err != nil {
			return false, false, false, err
		}
		madeProgress = true
	}
	if madeProgress {
		return true, false, false, nil
	}
	return false, len(inFlight) > 0, anyRollbackFailed, nil
}

// rollbackChild picks the rollback action for one ready child from its LIVE
// transfer.Outcome: Committed -> RequestReversal; Staged/Pending ->
// CancelStagedTransfer; InFlight (Accepted/Prepared) ->
// CancelAcceptedTransfer; never requested (only Gated, or no entry at all)
// -> nothing to call, straight to ABANDONED; already Failed/Cancelled on
// its own -> nothing left to undo, also ABANDONED.
func (s *Server) rollbackChild(ctx context.Context, transactionID string, spec *pb.Transfer, state childState) error {
	if state != childRequested && state != childCompleted {
		return s.appendSagaStep(ctx, transactionID, &pb.TransferRolledBackWithinTransaction{
			Id: transactionID, TransferId: spec.GetId(), Method: pb.RollbackMethod_ROLLBACK_METHOD_ABANDONED,
		})
	}

	outcome, err := transfer.Outcome(ctx, s.store, spec.GetId())
	if err != nil {
		return err
	}

	switch outcome {
	case transfer.OutcomeCommitted:
		reversalID := detid.New(transactionID + ":reversal:" + spec.GetId())
		resp, err := s.transfer.RequestReversal(ctx, &transferpb.RequestReversalRequest{
			Id: reversalID, TransferId: spec.GetId(), Reason: "transaction rollback",
			Stage: spec.GetStage(), TransactionId: transactionID,
		})
		if err != nil {
			return err
		}
		if rejected := resp.GetReversalRequestRejected(); rejected != nil {
			// Rejected at request time: the Reversal never started, so there is
			// nothing to wait for and this is already terminal.
			return s.appendSagaStep(ctx, transactionID, &pb.TransferRollbackFailedWithinTransaction{
				Id: transactionID, TransferId: spec.GetId(), Reason: rejected.GetReason(),
			})
		}
		// Accepted only means the Reversal's own saga has started. Record which
		// Reversal this child is now waiting on and stop; reconcileRollbacks
		// resolves it from that Reversal's live outcome. Deliberately NOT an
		// inline outcome read: that only ever worked because the Reversal's saga
		// ran to completion inside the call above, which stops being true once
		// Transfer's saga is driven asynchronously (go/docs/adr/0002). The
		// reversal id is deterministic, so a repeated sweep re-requests the same
		// Reversal rather than starting a second one.
		return s.appendSagaStep(ctx, transactionID, &pb.TransferReversalRequestedWithinTransaction{
			Id: transactionID, TransferId: spec.GetId(), ReversalId: reversalID,
		})

	case transfer.OutcomeStaged, transfer.OutcomePending:
		resp, err := s.transfer.CancelStagedTransfer(ctx, &transferpb.CancelStagedTransferRequest{Id: spec.GetId(), Reason: "transaction rollback"})
		if err != nil {
			return err
		}
		if rejected := resp.GetCancelStagedTransferRejected(); rejected != nil {
			return s.appendSagaStep(ctx, transactionID, &pb.TransferRollbackFailedWithinTransaction{
				Id: transactionID, TransferId: spec.GetId(), Reason: rejected.GetReason(),
			})
		}
		return s.appendSagaStep(ctx, transactionID, &pb.TransferRolledBackWithinTransaction{
			Id: transactionID, TransferId: spec.GetId(), Method: pb.RollbackMethod_ROLLBACK_METHOD_CANCELLED,
		})

	case transfer.OutcomeInFlight:
		if _, err := s.transfer.CancelAcceptedTransfer(ctx, &transferpb.CancelAcceptedTransferRequest{Id: spec.GetId(), Reason: "transaction rollback"}); err != nil {
			return err
		}
		return s.appendSagaStep(ctx, transactionID, &pb.TransferRolledBackWithinTransaction{
			Id: transactionID, TransferId: spec.GetId(), Method: pb.RollbackMethod_ROLLBACK_METHOD_CANCELLED,
		})

	case transfer.OutcomeFailed, transfer.OutcomeCancelled:
		// Already resolved on its own — nothing left to undo.
		return s.appendSagaStep(ctx, transactionID, &pb.TransferRolledBackWithinTransaction{
			Id: transactionID, TransferId: spec.GetId(), Method: pb.RollbackMethod_ROLLBACK_METHOD_ABANDONED,
		})

	default:
		return fmt.Errorf("transaction %q: rollbackChild: unexpected outcome %v for child %q", transactionID, outcome, spec.GetId())
	}
}
