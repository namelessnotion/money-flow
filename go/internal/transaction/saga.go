package transaction

import (
	"context"
	"errors"
	"fmt"
	"slices"

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
// recently landed — used to answer GetTransactionState/StartTransactionRollback
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
//
// A step the stream no longer allows (see stillDecidable) is dropped, not
// recorded: every caller re-folds afterwards and decides again from whatever
// overtook it.
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

	switch ok, err := stillDecidable(events, transactionID, event); {
	case err != nil:
		return err
	case !ok:
		return nil
	}

	// A child's outcome that decides the Transaction's own — the last child
	// completing, the last rollback landing — is appended together with that
	// outcome: nothing has to happen in between, so a second commit would
	// only cost another WAL flush (go/docs/adr/0010). Only a child's outcome:
	// an RPC's own decision (StartTransactionRollback) is recorded alone and
	// left for the orchestrator to act on (go/docs/adr/0006).
	pending := []proto.Message{event}
	if wantChildID != "" {
		after, err := withPending(events, transactionID, event)
		if err != nil {
			return err
		}
		switch concluded, err := conclusion(after, transactionID); {
		case err != nil:
			return err
		case concluded != nil:
			pending = append(pending, concluded)
		}
	}

	switch err := s.store.Append(ctx, AggregateType, transactionID, int64(len(events)), pending...); {
	case err == nil:
		return nil
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		return s.appendSagaStep(ctx, transactionID, event)
	default:
		return twirp.InternalErrorWith(err)
	}
}

// stillDecidable reports whether event may still be recorded on top of
// events. Every saga step is decided from a fold that may be stale by the time
// it is written — ADR 0001 lets a transfer-topic and a transaction-topic run
// fold the same Transaction at once, and an RPC can land between any fold and
// its append — so each step names the part of the fold that decided it, and
// that part must still hold:
//
//   - a forward child outcome, while the Transaction is Started and the child
//     is still Requested. Once rollback has begun, rollbackChild reads the
//     child's live outcome itself, so a late outcome is moot; recorded after
//     the rollback resolved the child it would rewrite it.
//   - TransactionRollbackStarted, while Initialized or Started. Nothing rolls
//     back from Completed.
//   - a child's rollback fact, while the Transaction is rolling back.
//   - a conclusion, while the stream still concludes exactly that.
//
// Dispatch intents (TransferRequested/GatedWithinTransaction) are not here:
// they are appended against the fold that chose them (recordIntents), because
// the side effect they license must not happen at all once they are stale.
func stillDecidable(events []eventstore.Event, transactionID string, event proto.Message) (bool, error) {
	top := topLevelState(events)
	switch event.(type) {
	case *pb.TransferCompletedWithinTransaction, *pb.TransferFailedWithinTransaction:
		if top != stateStarted {
			return false, nil
		}
		children, err := foldChildStates(events)
		if err != nil {
			return false, err
		}
		return children[childEventTransferID(event)] == childRequested, nil

	case *pb.TransactionRollbackStarted:
		return top == stateInitialized || top == stateStarted, nil

	case *pb.TransferReversalRequestedWithinTransaction, *pb.TransferRolledBackWithinTransaction, *pb.TransferRollbackFailedWithinTransaction:
		return top == stateRollbackStarted, nil

	case *pb.TransactionCompleted, *pb.TransactionRolledBack, *pb.TransactionRollbackFailed:
		concluded, err := conclusion(events, transactionID)
		if err != nil {
			return false, err
		}
		return concluded != nil && eventstore.EventType(concluded) == eventstore.EventType(event), nil

	default:
		return false, fmt.Errorf("transaction %q: %s is not a step appendSagaStep records", transactionID, eventstore.EventType(event))
	}
}

// withPending is events as they would read once event is appended after
// them, for deciding what event itself concludes.
func withPending(events []eventstore.Event, transactionID string, event proto.Message) ([]eventstore.Event, error) {
	payload, err := proto.Marshal(event)
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	return append(slices.Clone(events), eventstore.Event{
		AggregateType: AggregateType, AggregateID: transactionID, Sequence: int64(len(events)) + 1,
		EventType: eventstore.EventType(event), Payload: payload,
	}), nil
}

// conclusion is the Transaction's own outcome, if events alone decide it
// now, and nil if they do not: every child resolved and none failed while
// Started (TransactionCompleted), or nothing left to roll back and nothing
// still being reversed while RollingBack (TransactionRolledBack, or
// TransactionRollbackFailed if some child could not be). It is the one place
// those endings are decided, for runSaga and appendSagaStep alike.
func conclusion(events []eventstore.Event, transactionID string) (proto.Message, error) {
	switch topLevelState(events) {
	case stateStarted:
		transfers, _, err := decodeSpec(events)
		if err != nil {
			return nil, err
		}
		children, err := foldChildStates(events)
		if err != nil {
			return nil, err
		}
		if _, failed := firstFailed(children); failed || !allTerminal(transfers, children) {
			return nil, nil
		}
		return &pb.TransactionCompleted{Id: transactionID}, nil

	case stateRollbackStarted:
		transfers, deps, err := decodeSpec(events)
		if err != nil {
			return nil, err
		}
		children, err := foldChildStates(events)
		if err != nil {
			return nil, err
		}
		plan := planRollback(transfers, deps, children)
		if len(plan.candidates) > 0 || plan.inFlight {
			return nil, nil
		}
		reason, err := rollbackStartedReason(events)
		if err != nil {
			return nil, err
		}
		if plan.anyFailed {
			if reason == "" {
				reason = "one or more children could not be rolled back; see TransferRollbackFailedWithinTransaction"
			}
			return &pb.TransactionRollbackFailed{Id: transactionID, Reason: reason}, nil
		}
		return &pb.TransactionRolledBack{Id: transactionID, Reason: reason}, nil

	default:
		return nil, nil
	}
}

// Resume advances transactionID's saga from whatever its stream currently
// records — the Transaction-side twin of transfer.Server.Resume, and the other
// half of the event-triggered orchestrator's vocabulary. It is exactly
// runSaga, exported.
//
// It is now the only way a Transaction moves. Nothing in the RPC surface
// drives one, so a trigger the publication pipeline never delivers is a
// Transaction that waits indefinitely — cmd/resume exists for exactly that.
//
// Errors come straight back rather than being logged and swallowed: the
// orchestrator is the driver, and whether a trigger is retried or the
// consumer halts is its decision to make (go/docs/adr/0003).
func (s *Server) Resume(ctx context.Context, transactionID string) error {
	return s.runSaga(ctx, transactionID)
}

// runSaga folds the Transaction's current state and dispatches the next
// step, looping until it reaches a state that waits on something outside
// this call — an in-flight or gated child, or an external
// StartProcessingTransfer/StartTransactionRollback — a dispatch slice
// boundary (see maxDispatchPerStep), or a true terminal (Completed,
// RolledBack, RollbackFailed, Rejected). Safe, and expected, to call
// idempotently any number of times for the same id: each call resumes from
// wherever the stream actually left off.
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
			// Appended against the fold that decided it, not through
			// appendSagaStep's dedupe-and-retry. StartTransactionRollback may
			// land while a Transaction is still Initialized, and a Started
			// written after that would put it back to Started, undoing the
			// rollback and dispatching what it abandoned. On a conflict the
			// loop re-folds and decides again from whatever landed.
			switch err := s.store.Append(ctx, AggregateType, transactionID, int64(len(events)), &pb.TransactionStarted{Id: transactionID}); {
			case err == nil, errors.Is(err, eventstore.ErrConcurrencyConflict):
			default:
				return twirp.InternalErrorWith(err)
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

			progressed, err := s.reconcileInFlight(ctx, transactionID, transfers, children)
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

			switch concluded, err := conclusion(events, transactionID); {
			case err != nil:
				return err
			case concluded != nil:
				if err := s.appendSagaStep(ctx, transactionID, concluded); err != nil {
					return err
				}
				continue
			}

			dispatched, more, err := s.dispatchReady(ctx, transactionID, int64(len(events)), transfers, deps, children)
			if err != nil {
				return err
			}
			if more {
				// This slice is dispatched and its events are written. Those
				// events are themselves triggers, so the next resume takes the
				// next slice. Stop here rather than looping, so no one run's
				// fold is unbounded.
				return nil
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

			progressed, more, waiting, blocked, err := s.rollbackNext(ctx, transactionID, transfers, deps, children)
			if err != nil {
				return err
			}
			switch {
			case more:
				// Checked before blocked: more implies this slice made
				// progress, and evaluating blocked while children remain to be
				// rolled back would record TransactionRollbackFailed for a
				// rollback that is merely unfinished. The appends this slice
				// just made are the triggers that fetch the next one.
				return nil
			case progressed:
				continue
			case waiting:
				// At least one child is parked on a Reversal that has not
				// resolved. Nothing to decide yet, and nothing may be claimed:
				// stop here and resume when something asks again.
				return nil
			default:
				// Nothing left to roll back and nothing in flight: rolled back,
				// or — when blocked — stuck on a child that could not be.
				concluded, err := conclusion(events, transactionID)
				if err != nil {
					return err
				}
				if concluded == nil {
					return fmt.Errorf("transaction %q: rollback made no progress yet reached no conclusion (blocked=%v)", transactionID, blocked)
				}
				if err := s.appendSagaStep(ctx, transactionID, concluded); err != nil {
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

// maxDispatchPerStep bounds how many ready children one saga run dispatches
// before ending the run. It bounds the size of one unit of work, not the
// total: N children still cost N dispatches, they just cost them in
// ceil(N/K) separately-retryable runs instead of one unbounded fold. That is
// the point — a run is a Kafka handler, and a handler that keeps failing
// halts the consumer for every other aggregate sharing it (ADR 0003).
//
// No cursor is stored, because none is needed. Every dispatched child is
// recorded as a TransferRequested/GatedWithinTransaction intent on this
// Transaction's own stream, and readyToRun excludes every touched child: the
// stream is the cursor. Those same appends are published, so the writes that
// record a slice are the triggers that fetch the next one. A slice that
// reports more work therefore always appended at least one event — which is
// the whole progress argument, and is why it has a test of its own rather
// than only this comment.
//
// Per run, not in flight: ADR 0001 records that a transfer-topic and a
// transaction-topic message can be handled concurrently and both append
// here. A slice's intents are appended against the fold that chose them, so
// two runs that folded the same stream cannot both claim a slice from it: the
// loser re-folds and takes whatever is still ready. The absolute ceiling on a
// Transaction's width stays maxTransfersPerTransaction.
const maxDispatchPerStep = 8

// dispatchReady dispatches up to maxDispatchPerStep currently-ready children
// (per readyToRun): auto_process=true children get an actual RequestTransfer
// call; auto_process=false children are simply marked Gated, waiting for an
// explicit StartProcessingTransfer.
//
// Intent comes before effect (go/docs/adr/0011). The whole slice is recorded
// first, in one append against version — the length of the fold that chose
// it — and only then are its requests made. So a rollback that lands first
// makes the append lose, and nothing is requested; one that lands after finds
// every child it must undo already on the stream. On losing, dispatchReady
// reports dispatched so runSaga re-folds and decides again.
//
// Reports dispatched, so runSaga knows whether to loop again, and more, so it
// knows to end the run instead. more implies dispatched.
//
// Which children a slice draws is deliberately unspecified: readyToRun ranges
// a map, so the order varies between runs. Nothing depends on it — the DAG
// constrains a child only by its parents, and every ready child is by
// definition unconstrained.
func (s *Server) dispatchReady(
	ctx context.Context, transactionID string, version int64,
	transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList, children map[string]childState,
) (dispatched, more bool, err error) {
	ready := readyToRun(transfers, deps, touchedSet(children), completedSet(children))
	if len(ready) == 0 {
		return false, false, nil
	}
	if len(ready) > maxDispatchPerStep {
		ready, more = ready[:maxDispatchPerStep], true
	}

	intents := make([]proto.Message, 0, len(ready))
	for _, childID := range ready {
		if transfers[childID].GetAutoProcess() {
			intents = append(intents, &pb.TransferRequestedWithinTransaction{Id: transactionID, TransferId: childID})
		} else {
			intents = append(intents, &pb.TransferGatedWithinTransaction{Id: transactionID, TransferId: childID})
		}
	}
	switch recorded, err := s.recordIntents(ctx, transactionID, version, intents...); {
	case err != nil:
		return false, false, err
	case !recorded:
		return true, false, nil
	}

	for _, childID := range ready {
		if spec := transfers[childID]; spec.GetAutoProcess() {
			if _, err := s.requestChildTransfer(ctx, transactionID, spec); err != nil {
				return false, false, err
			}
		}
	}
	return true, more, nil
}

// recordIntents appends intents to transactionID's stream only if the stream
// is still at version, reporting whether they landed. Unlike appendSagaStep it
// neither dedupes nor retries: an intent licenses a side effect, so it may only
// be recorded against the exact fold that decided it. A caller whose append
// loses re-folds, and whatever overtook it — a rollback, or a concurrent run
// that dispatched the same children — is what it decides from.
func (s *Server) recordIntents(ctx context.Context, transactionID string, version int64, intents ...proto.Message) (bool, error) {
	switch err := s.store.Append(ctx, AggregateType, transactionID, version, intents...); {
	case err == nil:
		return true, nil
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		return false, nil
	default:
		return false, twirp.InternalErrorWith(err)
	}
}

// requestChildTransfer makes the request a TransferRequestedWithinTransaction
// already recorded the intent for: transfer.RequestTransfer for spec, tagged
// with transactionID. It is idempotent by the child's id, so completing an
// intent twice — a retried run, a rollback finishing one its driver
// abandoned — reaches the same one decision.
//
// An accept records nothing: the Transfer's own events report where it goes,
// and reconcileInFlight reads them. A rejection is recorded here, as
// TransferFailedWithinTransaction carrying the Transfer's reason, because a
// rejected Transfer names no Transaction (transfer.OwningTransaction) and so
// wakes none; it is returned too, for StartProcessingTransfer to answer with.
func (s *Server) requestChildTransfer(ctx context.Context, transactionID string, spec *pb.Transfer) (*transferpb.TransferRequestRejected, error) {
	resp, err := s.transfer.RequestTransfer(ctx, requestFor(transactionID, spec))
	if err != nil {
		return nil, err
	}
	rejected := resp.GetTransferRequestRejected()
	if rejected == nil {
		return nil, nil
	}
	if err := s.appendSagaStep(ctx, transactionID, &pb.TransferFailedWithinTransaction{
		Id: transactionID, TransferId: spec.GetId(), Reason: rejected.GetReason(),
	}); err != nil {
		return nil, err
	}
	return rejected, nil
}

func requestFor(transactionID string, spec *pb.Transfer) *transferpb.RequestTransferRequest {
	return &transferpb.RequestTransferRequest{
		Id: spec.GetId(), FromWalletId: spec.GetFromWalletId(), ToWalletId: spec.GetToWalletId(),
		Amount: spec.GetAmount(), Stage: spec.GetStage(), MintSource: spec.GetMintSource(),
		TransactionId: transactionID,
	}
}

// reconcileInFlight checks every currently-Requested child's live
// transfer.Outcome and records how it's resolved, if it has: Committed ->
// TransferCompletedWithinTransaction; Failed or Cancelled ->
// TransferFailedWithinTransaction. A child with no Transfer yet, or a
// rejected one, is an intent whose request has not been made or not been
// recorded — its driver stopped in between — so the request is completed
// here, which records a rejection. Anything else (InFlight, Staged, Pending)
// means still waiting on the outside world — no change, matching
// transfer.runSaga itself stopping at Staged/Pending. Returns true if it
// recorded anything, so runSaga knows to loop again.
func (s *Server) reconcileInFlight(
	ctx context.Context, transactionID string, transfers map[string]*pb.Transfer, children map[string]childState,
) (bool, error) {
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
		case transfer.OutcomeNotFound, transfer.OutcomeRejected:
			rejected, err := s.requestChildTransfer(ctx, transactionID, transfers[childID])
			if err != nil {
				return false, err
			}
			progressed = progressed || rejected != nil
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
) (progressed, more, waiting, blocked bool, err error) {
	plan := planRollback(transfers, deps, children)
	candidates := plan.candidates
	if len(candidates) > maxDispatchPerStep {
		candidates, more = candidates[:maxDispatchPerStep], true
	}

	madeProgress := false
	for _, childID := range candidates {
		if err := s.rollbackChild(ctx, transactionID, transfers[childID], children[childID]); err != nil {
			return false, false, false, false, err
		}
		madeProgress = true
	}
	if madeProgress {
		return true, more, false, false, nil
	}
	return false, false, plan.inFlight, plan.anyFailed, nil
}

// rollbackPlan is where a rollback stands, read off the fold alone.
type rollbackPlan struct {
	// candidates are the children rollbackChild should act on next.
	candidates []string
	// inFlight is true while some child's Reversal has not resolved.
	inFlight bool
	// anyFailed is true once some child has recorded a rollback failure.
	anyFailed bool
}

// planRollback reads a rollback's next move off the fold. Pure, so that
// rollbackNext acting on it and conclusion deciding the rollback is over
// cannot disagree.
func planRollback(transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList, children map[string]childState) rollbackPlan {
	rolledBack := make(map[string]bool, len(children))
	inFlight := make(map[string]bool, len(children))
	var plan rollbackPlan
	for id, st := range children {
		switch st {
		case childRolledBack:
			rolledBack[id] = true
		case childRollbackRequested:
			inFlight[id] = true
		case childRollbackFailed:
			plan.anyFailed = true
		}
	}
	plan.inFlight = len(inFlight) > 0

	ready := readyToRollback(transfers, deps, touchedSet(children), rolledBack, inFlight)

	// Filter before slicing, not inside the loop, and the difference is not
	// stylistic. A child already recorded as stuck is skipped without
	// appending anything — it is never retried automatically — so a slice
	// drawn from the unfiltered list can be made up entirely of those. This
	// call would then report no progress, fall through to blocked below, and
	// have runSaga record TransactionRollbackFailed while children that could
	// still have been reversed never were: money left out rather than put
	// back, and a Transaction that tells an operator it is beyond help when it
	// was only unfinished. Filtering first means blocked is reached only once
	// nothing rollbackable remains.
	//
	// readyToRun needs no equivalent: every child it returns appends exactly
	// once, so a forward slice always makes progress.
	for _, childID := range ready {
		if children[childID] != childRollbackFailed {
			plan.candidates = append(plan.candidates, childID)
		}
	}
	return plan
}

// rollbackChild picks the rollback action for one ready child from its LIVE
// transfer.Outcome: Committed -> RequestReversal; Staged/Pending ->
// CancelStagedTransfer; InFlight (Accepted/Prepared) ->
// CancelAcceptedTransfer; never requested (only Gated, or no entry at all)
// -> nothing to call, straight to ABANDONED; already Failed/Cancelled on
// its own, or rejected at accept -> nothing left to undo, also ABANDONED.
//
// A Requested child with no Transfer yet is an intent whose driver has not
// made the request, or is making it now (go/docs/adr/0011). The request is
// completed here — RequestTransfer is idempotent by id, so this and that
// driver reach the same one decision — and the child is then undone by what
// it became. Leaving it would let the driver's request land after the
// rollback concluded, with nothing left to undo it.
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
	case transfer.OutcomeNotFound:
		if _, err := s.transfer.RequestTransfer(ctx, requestFor(transactionID, spec)); err != nil {
			return err
		}
		return s.rollbackChild(ctx, transactionID, spec, state)

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
			var twerr twirp.Error
			if errors.As(err, &twerr) && twerr.Code() == twirp.FailedPrecondition {
				// The Transfer's own saga moved it past cancelling first — likely
				// for a request just completed above, whose acceptance set its
				// saga running. Undo it by what it became instead.
				return s.rollbackChild(ctx, transactionID, spec, state)
			}
			return err
		}
		return s.appendSagaStep(ctx, transactionID, &pb.TransferRolledBackWithinTransaction{
			Id: transactionID, TransferId: spec.GetId(), Method: pb.RollbackMethod_ROLLBACK_METHOD_CANCELLED,
		})

	case transfer.OutcomeFailed, transfer.OutcomeCancelled, transfer.OutcomeRejected:
		// Already resolved on its own — nothing left to undo.
		return s.appendSagaStep(ctx, transactionID, &pb.TransferRolledBackWithinTransaction{
			Id: transactionID, TransferId: spec.GetId(), Method: pb.RollbackMethod_ROLLBACK_METHOD_ABANDONED,
		})

	default:
		return fmt.Errorf("transaction %q: rollbackChild: unexpected outcome %v for child %q", transactionID, outcome, spec.GetId())
	}
}
