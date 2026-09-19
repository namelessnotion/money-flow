package transfer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	operationpb "github.com/namelessnotion/money_flow/go/gen/proto/operation/v1"
	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/detid"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/operation"
	"github.com/namelessnotion/money_flow/go/internal/token"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

// AggregateType is this aggregate's stream namespace in the event log.
// Shared by forward Transfers and Reversals — a Reversal is a new Transfer
// instance (decision #4), not a different aggregate type.
const AggregateType = "transfer"

// stagingTimeoutSeconds bounds how long a staged Transfer's TigerBeetle
// reservation is held before TigerBeetle auto-voids it if never posted or
// voided first — a safety net, not a normal path (decision #10), set well
// beyond the ACH-style 1-3 day settlement window it exists to cover.
const stagingTimeoutSeconds = 10 * 24 * 60 * 60 // 10 days

type transferState int

const (
	stateUnknown transferState = iota
	stateAccepted
	statePrepared
	stateStaged
	statePending
	stateCommitted
	stateFailed
	stateCancelled
)

func (s transferState) String() string {
	switch s {
	case stateAccepted:
		return "accepted"
	case statePrepared:
		return "prepared"
	case stateStaged:
		return "staged"
	case statePending:
		return "pending"
	case stateCommitted:
		return "committed"
	case stateFailed:
		return "failed"
	case stateCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// stateByEventType maps every event type that advances a Transfer's (or
// Reversal's) saga to the state it produces. This is the one place that
// list is written down: currentState() folds it, and claimForDispatch's
// marker scan (go/docs/adr/0005) uses the same map to tell a real
// transition apart from incidental stream noise — a client-visible
// *Rejected response (ConfirmStagedTransfer, CancelStagedTransfer,
// PostPendingTransfer all record one onto the Transfer's own stream when
// called against a state that doesn't match) does not appear here, on
// purpose: it never changes the fold, and a claim scan that stopped at one
// as if it were a real transition would miss a still-live claim sitting
// underneath it — go/docs/adr/0005 was found doing exactly that.
var stateByEventType = map[string]transferState{
	eventstore.EventType(&pb.TransferRequestAccepted{}):    stateAccepted,
	eventstore.EventType(&pb.ReversalRequestAccepted{}):    stateAccepted,
	eventstore.EventType(&pb.TransferPrepared{}):           statePrepared,
	eventstore.EventType(&pb.TransferStaged{}):             stateStaged,
	eventstore.EventType(&pb.TransferPending{}):            statePending,
	eventstore.EventType(&pb.TransferCommitted{}):          stateCommitted,
	eventstore.EventType(&pb.TransferFailed{}):              stateFailed,
	eventstore.EventType(&pb.TransferCancelled{}):           stateCancelled,
	eventstore.EventType(&pb.AcceptedTransferCancelled{}):  stateCancelled,
	eventstore.EventType(&pb.PreparedTransferCancelled{}):  stateCancelled,
}

// currentState folds a Transfer's (or Reversal's — same aggregate type and
// saga shape) stream to find which state it's currently in, based on the
// last saga-outcome event recorded. This implementation emits exactly one
// event per saga transition (the outcome itself — TransferPrepared,
// TransferStaged, TransferCommitted, and so on) rather than every
// Start/*Started/Complete triplet the proto defines: nothing observed the
// in-between state in the original synchronous, single-process saga
// (decision #5); go/docs/adr/0005 later put those markers to work as
// dispatch claims, but currentState() still ignores them deliberately —
// see that ADR for why moving a claim into the fold would break crash
// recovery.
func currentState(events []eventstore.Event) transferState {
	state := stateUnknown
	for _, e := range events {
		if s, ok := stateByEventType[e.EventType]; ok {
			state = s
		}
	}
	return state
}

// OutcomeKind is a coarse, exported summary of a Transfer's (or Reversal's)
// current state — the read-only cross-aggregate view Transaction's saga
// needs to reconcile an in-flight child or pick a rollback action, without
// exposing transfer's own unexported transferState enum.
type OutcomeKind int

const (
	OutcomeNotFound OutcomeKind = iota
	OutcomeRejected
	OutcomeInFlight // Accepted or Prepared
	OutcomeStaged
	OutcomePending
	OutcomeCommitted
	OutcomeFailed
	OutcomeCancelled
)

func (o OutcomeKind) String() string {
	switch o {
	case OutcomeNotFound:
		return "not_found"
	case OutcomeRejected:
		return "rejected"
	case OutcomeInFlight:
		return "in_flight"
	case OutcomeStaged:
		return "staged"
	case OutcomePending:
		return "pending"
	case OutcomeCommitted:
		return "committed"
	case OutcomeFailed:
		return "failed"
	case OutcomeCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// Outcome folds transferID's own stream (a Transfer's or a Reversal's — same
// aggregate type) into a coarse, exported summary. Read-only: it never
// drives the saga forward, only reports where the stream currently stands.
// A rejection is reported straight off the opening event rather than folded
// — see rejectedRequest.
func Outcome(ctx context.Context, store eventstore.Store, transferID string) (OutcomeKind, error) {
	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return OutcomeNotFound, twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return OutcomeNotFound, nil
	}
	if rejectedRequest(events) {
		return OutcomeRejected, nil
	}

	switch currentState(events) {
	case stateAccepted, statePrepared:
		return OutcomeInFlight, nil
	case stateStaged:
		return OutcomeStaged, nil
	case statePending:
		return OutcomePending, nil
	case stateCommitted:
		return OutcomeCommitted, nil
	case stateFailed:
		return OutcomeFailed, nil
	case stateCancelled:
		return OutcomeCancelled, nil
	default:
		return OutcomeNotFound, nil
	}
}

// rejectedRequest reports whether events is a rejected request's stream. A
// rejection is the decision not to start a saga at all, so it is always event
// 0 and nothing can ever follow it: the fact is read off the opening event
// rather than folded, since currentState has no vocabulary for it — its states
// are the accepted-through-committed progression a rejection never enters.
// events must be non-empty.
func rejectedRequest(events []eventstore.Event) bool {
	switch events[0].EventType {
	case eventstore.EventType(&pb.TransferRequestRejected{}), eventstore.EventType(&pb.ReversalRequestRejected{}):
		return true
	default:
		return false
	}
}

// stageRequested reads the stage flag off a Transfer's (or Reversal's)
// Accepted event — the one place that fact is durably recorded, so every
// resume from the event log (not just the original synchronous call) can
// answer it. events must be non-empty.
func stageRequested(events []eventstore.Event) (bool, error) {
	msg, err := events[0].Decode()
	if err != nil {
		return false, twirp.InternalErrorWith(err)
	}
	switch m := msg.(type) {
	case *pb.TransferRequestAccepted:
		return m.GetStage(), nil
	case *pb.ReversalRequestAccepted:
		return m.GetStage(), nil
	default:
		return false, fmt.Errorf("transfer: stream starts with %s, want an Accepted event", events[0].EventType)
	}
}

// preparedLegs returns the leg manifest TransferPrepared recorded for
// transferID.
func preparedLegs(events []eventstore.Event, transferID string) ([]*pb.TransferLeg, error) {
	for _, e := range events {
		if e.EventType != eventstore.EventType(&pb.TransferPrepared{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		prepared, ok := msg.(*pb.TransferPrepared)
		if !ok {
			return nil, twirp.InternalError(fmt.Sprintf(
				"transfer %q: event typed %s did not decode as TransferPrepared", transferID, e.EventType,
			))
		}
		return prepared.GetLegs(), nil
	}
	return nil, fmt.Errorf("transfer %q: no TransferPrepared event found", transferID)
}

// forEachOperation calls fn once for every DEBIT Operation's id in legs,
// then once for every distinct CREDIT Operation's id (a CREDIT is shared
// across every leg feeding the same destination Token). Centralizes that
// DEBIT-then-CREDIT, dedup-CREDIT ordering for stage/commit/cancelStaged/
// compensate, which all walk the same shape.
func forEachOperation(legs []*pb.TransferLeg, fn func(operationID string) error) error {
	for _, leg := range legs {
		if err := fn(leg.GetDebitOperationId()); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(legs))
	for _, leg := range legs {
		creditID := leg.GetCreditOperationId()
		if seen[creditID] {
			continue
		}
		seen[creditID] = true
		if err := fn(creditID); err != nil {
			return err
		}
	}
	return nil
}

// buildDestinations groups legs by destination Token, summing the amount
// each receives, for the TransferCommitted summary.
func buildDestinations(legs []*pb.TransferLeg) []*pb.TransferDestination {
	var order []string
	sums := make(map[string]uint64, len(legs))
	creditOps := make(map[string]string, len(legs))
	currency := ""
	for _, leg := range legs {
		destID := leg.GetDestTokenId()
		if _, seen := sums[destID]; !seen {
			order = append(order, destID)
			creditOps[destID] = leg.GetCreditOperationId()
		}
		sums[destID] += leg.GetAmount().GetMinorUnits()
		currency = leg.GetAmount().GetCurrency()
	}
	destinations := make([]*pb.TransferDestination, len(order))
	for i, destID := range order {
		destinations[i] = &pb.TransferDestination{
			ToTokenId: destID, Amount: &sharedpb.Money{MinorUnits: sums[destID], Currency: currency},
			CreditOperationId: creditOps[destID],
		}
	}
	return destinations
}

// loadLegs loads transferID's stream and returns its recorded leg manifest
// — the "load, then read TransferPrepared back out" pair that stage,
// cancelStaged, and cancelPrepared all start with — plus its currently-
// folded state, which claimForDispatch (go/docs/adr/0005) needs as
// preClaimState.
func (s *Server) loadLegs(ctx context.Context, transferID string) (legs []*pb.TransferLeg, state transferState, err error) {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return nil, stateUnknown, twirp.InternalErrorWith(err)
	}
	legs, err = preparedLegs(events, transferID)
	if err != nil {
		return nil, stateUnknown, err
	}
	return legs, currentState(events), nil
}

// errBatchRejected signals that submitBatch's batch was rejected and
// onReject already handled the consequence — a caller must always stop on
// this, never only on a non-nil error from onReject itself. onReject
// returning nil means "I recorded the rejection cleanly" (compensate()
// legitimately returns nil after appending TransferFailed), not "there was
// nothing to record" — conflating the two used to let stage()/commit() fall
// through to operation.Stage()/Perform() on an Operation compensate() had
// just marked Failed, the exact contradiction operation/server.go's
// terminal-state guard exists to catch.
var errBatchRejected = errors.New("transfer: batch rejected")

// submitBatch submits batch to TigerBeetle and checks every result, calling
// onReject for the first leg that isn't OK or Exists, then records the new
// balance of every Token the batch touched (token.RecordBalances) — stage, commit, and
// cancelStaged all submit a batch this same way and only differ in what
// "rejected" means for them (stage/commit route it to compensate() as an
// internal-invariant Failed; cancelStaged treats it as a plain internal
// error, since a void being rejected isn't decision #13's Cancelled/Failed
// split at all — see cancelStaged's own comment).
func (s *Server) submitBatch(
	ctx context.Context, batch []ledger.Transfer, onReject func(legIndex int, result ledger.TransferResultCode) error,
) error {
	results, err := s.ledger.CreateTransfers(ctx, batch)
	if err != nil {
		return twirp.InternalErrorWith(fmt.Errorf("ledger: %w", err))
	}
	for i, r := range results {
		if r.Result != ledger.TransferResultOK && r.Result != ledger.TransferResultExists {
			if err := onReject(i, r.Result); err != nil {
				return err
			}
			return errBatchRejected
		}
	}
	// Publish every touched Token's new balance before the saga step's own
	// event: if this fails, the step is retried, TigerBeetle answers Exists,
	// and the recording runs again — at-least-once without a gap.
	touched := make([]string, 0, 2*len(batch))
	for _, t := range batch {
		touched = append(touched, t.DebitAccountID, t.CreditAccountID)
	}
	return token.RecordBalances(ctx, s.store, s.ledger, touched)
}

// appendSagaStep appends event as the next fact on transferID's own stream,
// unless it's already there (idempotent convergence — a retried saga step,
// or another concurrent call driving the same Transfer, already recorded
// it).
func (s *Server) appendSagaStep(ctx context.Context, transferID string, event proto.Message) error {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return twirp.InternalErrorWith(err)
	}
	wantType := eventstore.EventType(event)
	if len(events) > 0 && events[len(events)-1].EventType == wantType {
		return nil
	}
	switch err := s.store.Append(ctx, AggregateType, transferID, int64(len(events)), event); {
	case err == nil:
		return nil
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		return s.appendSagaStep(ctx, transferID, event)
	default:
		return twirp.InternalErrorWith(err)
	}
}

const claimPollInterval = 20 * time.Millisecond

// claimStaleAfter is how old a claim marker (go/docs/adr/0005) has to be
// before a caller treats it as abandoned — its winner crashed before
// finishing — rather than still active. A var, not a const: tests that seed
// an orphaned marker directly shrink it so crash-recovery tests don't have
// to sleep for real. Generous relative to a normal TigerBeetle round trip
// (single-digit milliseconds in practice), tight relative to cmd/simulate's
// own 30s per-RPC timeout.
var claimStaleAfter = 5 * time.Second

// isClaimMarkerType reports whether eventType is one of the four claim
// markers stage()/commit()/cancelStaged()/cancelPrepared() append.
func isClaimMarkerType(eventType string) bool {
	switch eventType {
	case eventstore.EventType(&pb.StagingTransferStarted{}),
		eventstore.EventType(&pb.TransferCommittingStarted{}),
		eventstore.EventType(&pb.CancellingStagedTransferStarted{}),
		eventstore.EventType(&pb.CancellingPreparedTransferStarted{}):
		return true
	default:
		return false
	}
}

// liveMarker scans events from the end for the most recent claim marker
// that is still live — nothing that advances currentState()'s fold
// (stateByEventType) has been appended after it. It does not stop at the
// literal last event: a client-visible *Rejected response
// (ConfirmStagedTransfer, CancelStagedTransfer, PostPendingTransfer all
// record one onto the Transfer's own stream when called against a state
// that doesn't match what the caller expected) can land after a live
// marker without advancing the fold, and a caller that only checked the
// trailing event would see the rejection, conclude nothing was claimed,
// and take a second claim while the first is still in flight — exactly
// the gap go/docs/adr/0005 was found to have.
func liveMarker(events []eventstore.Event) (eventstore.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if isClaimMarkerType(e.EventType) {
			return e, true
		}
		if _, advances := stateByEventType[e.EventType]; advances {
			return eventstore.Event{}, false
		}
	}
	return eventstore.Event{}, false
}

// claimForDispatch is the mutual-exclusion gate for a side-effecting saga
// step (go/docs/adr/0005). It is deliberately not just one eventstore.Claim
// call at the stream's current tail: a caller that reloads the stream
// *after* a marker has landed, but before its winner has finished, must
// wait for that marker to resolve rather than immediately claiming the
// *next* sequence number — an earlier version of this fix got this wrong
// (twice — see the ADR), and a claim on the next slot runs concurrently
// with the still-in-flight first one against the same Operations,
// reproducing the exact contradiction this decision exists to prevent.
//
// A marker older than claimStaleAfter is treated as abandoned and is safe
// to re-claim with the same marker type — this is what preserves crash
// recovery without a lease or a heartbeat: currentState() still ignores
// every marker, so re-claiming just re-enters the same, already-idempotent
// dispatch path. A different transition is refused (errAbandonedClaim)
// rather than let through: "abandoned" is a guess, the old winner may be
// merely slow and mid-step, and even if it did crash it may have
// half-applied its step (some Operations Cancelled, a TigerBeetle void
// landed) — the same transition converges over that, a different one
// contradicts it. runSaga finishes an abandoned cancel of a Prepared
// Transfer itself (claimedPreparedCancel), so the orchestrator never
// trips this refusal on its own dispatch. Staleness is
// measured on the store's own clock (store.Now), not the Go process's:
// PostgresStore's occurred_at is set by Postgres's now(), and comparing it
// against a different machine's clock can make every marker look
// instantly stale, or never stale, depending on which way the clocks
// drift.
//
// This alone does not make it safe for the original claimant to run its
// side effects unconditionally once won=true: a marker only goes stale
// after claimStaleAfter, but that is a guess about crashes, not a
// guarantee the original caller is actually gone. A legitimately slow
// step (this codebase has already measured multi-second Postgres write
// latency under load) can still be reclaimed out from under its own
// caller. stillHoldsClaim, called by stage()/commit()/cancelStaged()/
// cancelPrepared()/compensate() immediately before each externally visible
// action, stops the slow winner at its next step. It cannot stop it mid-step
// (inside a multi-leg Operation loop, or a TigerBeetle round trip), which is
// why only the same marker type may take a stale claim over: that overlap
// converges on idempotent writes, a different transition's would not.
//
// won=false, err=nil means the aggregate already moved past preClaimState —
// by this call's own eventual claim below, or a concurrent one — and the
// caller has nothing left to do; it should return nil unconditionally,
// never retry a fresh claim itself. On won=true, claimedSeq is the
// sequence number the marker landed at, for stillHoldsClaim.
func (s *Server) claimForDispatch(ctx context.Context, transferID string, preClaimState transferState, marker proto.Message) (claimedSeq int64, won bool, err error) {
	for {
		events, err := s.store.Load(ctx, AggregateType, transferID)
		if err != nil {
			return 0, false, twirp.InternalErrorWith(err)
		}
		if currentState(events) != preClaimState {
			return 0, false, nil
		}
		if live, ok := liveMarker(events); ok {
			now, err := s.store.Now(ctx)
			if err != nil {
				return 0, false, twirp.InternalErrorWith(err)
			}
			if now.Sub(live.OccurredAt) < claimStaleAfter {
				select {
				case <-ctx.Done():
					return 0, false, twirp.InternalErrorWith(ctx.Err())
				case <-time.After(claimPollInterval):
				}
				continue
			}
			if live.EventType != eventstore.EventType(marker) {
				return 0, false, abandonedClaimError(transferID, live.EventType, marker)
			}
		}

		expectedSeq := int64(len(events))
		won, err := eventstore.Claim(ctx, s.store, AggregateType, transferID, expectedSeq, marker)
		if err != nil {
			return 0, false, twirp.InternalErrorWith(err)
		}
		if won {
			return expectedSeq + 1, true, nil
		}
		// Lost the CAS for this exact slot to a concurrent claimant; loop
		// and re-evaluate from scratch rather than assuming who won it.
	}
}

// errAbandonedClaim marks claimForDispatch refusing to take over a stale
// claim that belongs to a different transition (go/docs/adr/0005). Only
// that same transition may resume it.
var errAbandonedClaim = errors.New("transfer: abandoned claim belongs to a different transition")

// abandonedClaimError is errAbandonedClaim as a FailedPrecondition, so an
// RPC caller learns this Transfer is waiting on another transition to be
// retried, not that the server failed.
func abandonedClaimError(transferID, abandonedMarkerType string, wanted proto.Message) error {
	return twirp.WrapError(twirp.NewError(twirp.FailedPrecondition, fmt.Sprintf(
		"transfer %q: an abandoned %s claim can only be resumed by that same transition, not %s",
		transferID, abandonedMarkerType, eventstore.EventType(wanted),
	)), errAbandonedClaim)
}

// claimedPreparedCancel reports whether events' live claim marker
// (liveMarker) is a CancellingPreparedTransferStarted, and its reason. A
// Prepared Transfer carrying one is being cancelled, not staged or
// committed: runSaga must finish that cancel (the claimant crashed, or is
// still working and cancelPrepared will wait on it) rather than dispatch
// its usual next step, which claimForDispatch would refuse.
func claimedPreparedCancel(events []eventstore.Event) (reason string, ok bool, err error) {
	live, ok := liveMarker(events)
	if !ok || live.EventType != eventstore.EventType(&pb.CancellingPreparedTransferStarted{}) {
		return "", false, nil
	}
	msg, err := live.Decode()
	if err != nil {
		return "", false, twirp.InternalErrorWith(err)
	}
	marker, ok := msg.(*pb.CancellingPreparedTransferStarted)
	if !ok {
		return "", false, twirp.InternalError(fmt.Sprintf("claim marker decoded as %T", msg))
	}
	return marker.GetReason(), true, nil
}

// stillHoldsClaim reports whether transferID's live marker (liveMarker) is
// still the one this caller appended at claimedSeq — called immediately
// before each externally visible action in stage()/commit()/cancelStaged()/
// cancelPrepared()/compensate() (go/docs/adr/0005: a claim can be
// legitimately reclaimed out from under a caller that is merely slow, not
// crashed, so "I won the claim earlier" is not enough — a caller has to
// keep checking it still holds as its own work takes real time). false
// means stop immediately and return nil: someone else now owns this step.
func (s *Server) stillHoldsClaim(ctx context.Context, transferID string, claimedSeq int64) (bool, error) {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return false, twirp.InternalErrorWith(err)
	}
	live, ok := liveMarker(events)
	return ok && live.Sequence == claimedSeq, nil
}

// errClaimSuperseded signals that requireClaim found this caller's claim no
// longer live: a concurrent caller reclaimed it after treating it as
// abandoned (go/docs/adr/0005). Every call site treats this exactly like
// losing claimForDispatch's own initial race — stop immediately and return
// nil, never proceed to the action that was about to run.
var errClaimSuperseded = errors.New("transfer: claim superseded")

// requireClaim wraps stillHoldsClaim for the early-return shape every
// side-effecting checkpoint in stage()/commit()/cancelStaged()/
// cancelPrepared()/compensate() needs: nil means still held, proceed;
// errClaimSuperseded means stop, the caller should return nil; anything
// else is a real error to propagate.
func (s *Server) requireClaim(ctx context.Context, transferID string, claimedSeq int64) error {
	held, err := s.stillHoldsClaim(ctx, transferID, claimedSeq)
	if err != nil {
		return err
	}
	if !held {
		return errClaimSuperseded
	}
	return nil
}

// prepare mints the destination Token(s) (skipped for a reversal — its
// destinations are always the original Transfer's own, pre-existing source
// Tokens) and initiates every leg's Operations, all in one AppendAtomic —
// the Transfer's own stream, any new Token stream(s), and every new
// Operation stream are all *created together*, the same shape as
// Holder.Provision.
func (s *Server) prepare(ctx context.Context, transferID string) error {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return fmt.Errorf("transfer %q: no accepted event to prepare from", transferID)
	}
	msg, err := events[0].Decode()
	if err != nil {
		return twirp.InternalErrorWith(err)
	}

	var legs []Leg
	var mintWrites []eventstore.StreamWrite

	switch accepted := msg.(type) {
	case *pb.TransferRequestAccepted:
		var srcLegs []Leg
		var srcMintWrites []eventstore.StreamWrite

		if accepted.GetMintSource() {
			// Re-validate the same way selectSourceTokens is re-validated
			// below, in case anything changed between accept and prepare —
			// defensive, mirroring the existing "re-selection failed after
			// accept" pattern.
			rejection, err := validateMintSource(ctx, s.store, s.transactionExists, accepted.GetTransactionId(), accepted.GetFromWalletId())
			if err != nil {
				return err
			}
			if rejection != nil {
				return fmt.Errorf("transfer %q: prepare: mint_source re-validation failed after accept: %s", transferID, rejection.GetReason())
			}

			srcSpec := mintSourceLeg(accepted.GetAmount())
			srcWalletEvents, err := s.store.Load(ctx, wallet.AggregateType, accepted.GetFromWalletId())
			if err != nil {
				return twirp.InternalErrorWith(err)
			}
			writes, mintRejection, err := token.MintWrites(
				ctx, s.store, s.ledger, accepted.GetFromWalletId(), srcWalletEvents,
				[]token.MintSpec{srcSpec}, accepted.GetTransactionId(),
			)
			if err != nil {
				return err
			}
			if mintRejection != nil {
				return fmt.Errorf("transfer %q: prepare: source mint rejected: %s", transferID, mintRejection.GetReason())
			}
			srcMintWrites = writes
			srcLegs = []Leg{{SourceTokenID: srcSpec.TokenID, Amount: accepted.GetAmount()}}
		} else {
			selected, rejection, err := selectSourceTokens(
				ctx, s.store, s.ledger, accepted.GetFromWalletId(), accepted.GetAmount(),
				accepted.GetTransactionId(), s.isOpen,
			)
			if err != nil {
				return err
			}
			if rejection != nil {
				return fmt.Errorf("transfer %q: prepare: re-selection failed after accept: %s", transferID, rejection.GetReason())
			}
			srcLegs = selected
		}

		destSpecs := planDestinations(accepted.GetAmount())
		walletEvents, err := s.store.Load(ctx, wallet.AggregateType, accepted.GetToWalletId())
		if err != nil {
			return twirp.InternalErrorWith(err)
		}
		writes, mintRejection, err := token.MintWrites(
			ctx, s.store, s.ledger, accepted.GetToWalletId(), walletEvents, destSpecs, accepted.GetTransactionId(),
		)
		if err != nil {
			return err
		}
		if mintRejection != nil {
			return fmt.Errorf("transfer %q: prepare: mint rejected: %s", transferID, mintRejection.GetReason())
		}
		mintWrites = append(srcMintWrites, writes...)

		destTokenID := destSpecs[0].TokenID
		for i := range srcLegs {
			srcLegs[i].DestTokenID = destTokenID
		}
		legs = srcLegs

	case *pb.ReversalRequestAccepted:
		revLegs, rejection, err := reversalManifest(ctx, s.store, accepted.GetTransferId())
		if err != nil {
			return err
		}
		if rejection != nil {
			return fmt.Errorf("transfer %q: prepare: reversal manifest failed after accept: %s", transferID, rejection.GetReason())
		}
		legs = revLegs

	default:
		return fmt.Errorf("transfer %q: stream starts with %s, want an Accepted event", transferID, events[0].EventType)
	}

	writes := make([]eventstore.StreamWrite, 0, len(mintWrites)+2*len(legs)+1)
	writes = append(writes, mintWrites...)

	protoLegs := make([]*pb.TransferLeg, len(legs))
	creditOpByDest := make(map[string]string, len(legs))
	amountByDest := make(map[string]uint64, len(legs))
	currency := ""
	for i, leg := range legs {
		debitOpID := uuid.NewV7().String()
		writes = append(writes, eventstore.StreamWrite{
			AggregateType: operation.AggregateType, AggregateID: debitOpID, ExpectedSeq: 0,
			Events: []proto.Message{operation.InitiatedEvent(
				debitOpID, transferID, leg.SourceTokenID, leg.DestTokenID, operationpb.Operator_OPERATOR_DEBIT, leg.Amount,
			)},
		})

		creditOpID, ok := creditOpByDest[leg.DestTokenID]
		if !ok {
			creditOpID = uuid.NewV7().String()
			creditOpByDest[leg.DestTokenID] = creditOpID
		}
		amountByDest[leg.DestTokenID] += leg.Amount.GetMinorUnits()
		currency = leg.Amount.GetCurrency()

		protoLegs[i] = &pb.TransferLeg{
			SourceTokenId: leg.SourceTokenID, DestTokenId: leg.DestTokenID, Amount: leg.Amount,
			DebitOperationId: debitOpID, CreditOperationId: creditOpID,
		}
	}
	for destID, creditOpID := range creditOpByDest {
		writes = append(writes, eventstore.StreamWrite{
			AggregateType: operation.AggregateType, AggregateID: creditOpID, ExpectedSeq: 0,
			Events: []proto.Message{operation.InitiatedEvent(
				creditOpID, transferID, destID, "", operationpb.Operator_OPERATOR_CREDIT,
				&sharedpb.Money{MinorUnits: amountByDest[destID], Currency: currency},
			)},
		})
	}

	writes = append(writes, eventstore.StreamWrite{
		AggregateType: AggregateType, AggregateID: transferID, ExpectedSeq: int64(len(events)),
		Events: []proto.Message{&pb.TransferPrepared{Id: transferID, Legs: protoLegs}},
	})

	switch err := s.store.AppendAtomic(ctx, writes...); {
	case err == nil:
		return nil
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		events, err := s.store.Load(ctx, AggregateType, transferID)
		if err != nil {
			return twirp.InternalErrorWith(err)
		}
		for _, e := range events {
			if e.EventType == eventstore.EventType(&pb.TransferPrepared{}) {
				return nil // a concurrent prepare already landed
			}
		}
		return fmt.Errorf("transfer %q: prepare conflicted and did not converge", transferID)
	default:
		return twirp.InternalErrorWith(err)
	}
}

// stage submits every leg's DEBIT as a TigerBeetle pending transfer
// (reserving capacity, posting nothing), then stages every DEBIT then every
// distinct CREDIT Operation, then appends TransferStaged. A TigerBeetle-
// level rejection here is our own ledger's invariant failing, so it routes
// to compensate() (Failed), not cancelStaged() (Cancelled) — see decision
// #13.
//
// Claims StagingTransferStarted before touching TigerBeetle (go/docs/adr/0005):
// the synchronous RPC path and the async orchestrator's Resume both reach
// here from the same statePrepared read, with nothing else stopping them
// from both submitting to TigerBeetle at once. A caller that loses the claim
// (claimForDispatch) has nothing left to do — the transition already
// happened, by this call or another. A caller that won it re-checks
// requireClaim before each externally visible action, since a claim can be
// reclaimed out from under a merely-slow (not crashed) winner.
func (s *Server) stage(ctx context.Context, transferID string) error {
	legs, _, err := s.loadLegs(ctx, transferID)
	if err != nil {
		return err
	}
	claimedSeq, won, err := s.claimForDispatch(ctx, transferID, statePrepared, &pb.StagingTransferStarted{Id: transferID})
	if err != nil {
		return err
	}
	if !won {
		return nil
	}

	batch := make([]ledger.Transfer, len(legs))
	for i, leg := range legs {
		batch[i] = ledger.Transfer{
			ID: leg.GetDebitOperationId(), DebitAccountID: leg.GetSourceTokenId(), CreditAccountID: leg.GetDestTokenId(),
			MinorUnits: leg.GetAmount().GetMinorUnits(), Currency: leg.GetAmount().GetCurrency(),
			Kind: ledger.TransferKindPending, Timeout: stagingTimeoutSeconds,
			Linked: i < len(legs)-1,
		}
	}
	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	switch err := s.submitBatch(ctx, batch, func(legIndex int, result ledger.TransferResultCode) error {
		return s.compensate(ctx, transferID, claimedSeq, fmt.Sprintf("tigerbeetle rejected staging leg %d: %v", legIndex, result))
	}); {
	case err == nil:
	case errors.Is(err, errBatchRejected):
		return nil // compensate() already recorded TransferFailed
	case errors.Is(err, errClaimSuperseded):
		return nil // compensate() found itself superseded before recording anything
	default:
		return err
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	if err := forEachOperation(legs, func(operationID string) error {
		_, err := operation.Stage(ctx, s.store, operationID)
		return err
	}); err != nil {
		return err
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	return s.appendSagaStep(ctx, transferID, &pb.TransferStaged{Id: transferID})
}

// confirmStaged records TransferPending — a pure event-log write, called
// only from ConfirmStagedTransfer. No TigerBeetle call: the reservation
// already exists from stage().
func (s *Server) confirmStaged(ctx context.Context, transferID string) error {
	return s.appendSagaStep(ctx, transferID, &pb.TransferPending{Id: transferID})
}

// commit submits every leg's DEBIT to TigerBeetle as one linked batch, then
// performs every DEBIT then every distinct CREDIT Operation, then appends
// TransferCommitted. From Prepared (the immediate, non-staged path) this is
// a fresh transfer batch; from Pending (called via PostPendingTransfer)
// it's a post_pending_transfer batch referencing each leg's already-staged
// TigerBeetle transfer — "posted" and "committed" are the same terminal
// state reached by two different routes. A TigerBeetle-level rejection
// routes to compensate() (Failed) either way.
//
// Claims TransferCommittingStarted before touching TigerBeetle, for the same
// reason stage() does (go/docs/adr/0005): PostPendingTransfer can be called
// while the orchestrator's Resume is independently dispatching the same
// Transfer from Prepared.
func (s *Server) commit(ctx context.Context, transferID string) error {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return twirp.InternalErrorWith(err)
	}
	legs, err := preparedLegs(events, transferID)
	if err != nil {
		return err
	}
	// commit() needs the state itself (to pick immediate vs. posting mode,
	// and as claimForDispatch's preClaimState below), which loadLegs
	// doesn't expose — the one of the four callers that can't use it.
	preClaimState := currentState(events)
	posting := preClaimState == statePending

	claimedSeq, won, err := s.claimForDispatch(ctx, transferID, preClaimState, &pb.TransferCommittingStarted{Id: transferID})
	if err != nil {
		return err
	}
	if !won {
		return nil
	}

	batch := make([]ledger.Transfer, len(legs))
	for i, leg := range legs {
		t := ledger.Transfer{
			DebitAccountID: leg.GetSourceTokenId(), CreditAccountID: leg.GetDestTokenId(),
			MinorUnits: leg.GetAmount().GetMinorUnits(), Currency: leg.GetAmount().GetCurrency(),
			Linked: i < len(legs)-1,
		}
		if posting {
			t.ID = detid.New(leg.GetDebitOperationId() + ":post")
			t.Kind = ledger.TransferKindPostPending
			t.PendingID = leg.GetDebitOperationId()
		} else {
			t.ID = leg.GetDebitOperationId()
			t.Kind = ledger.TransferKindRegular
		}
		batch[i] = t
	}
	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	switch err := s.submitBatch(ctx, batch, func(legIndex int, result ledger.TransferResultCode) error {
		return s.compensate(ctx, transferID, claimedSeq, fmt.Sprintf("tigerbeetle rejected commit leg %d: %v", legIndex, result))
	}); {
	case err == nil:
	case errors.Is(err, errBatchRejected):
		return nil // compensate() already recorded TransferFailed
	case errors.Is(err, errClaimSuperseded):
		return nil // compensate() found itself superseded before recording anything
	default:
		return err
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	if err := forEachOperation(legs, func(operationID string) error {
		_, err := operation.Perform(ctx, s.store, operationID)
		return err
	}); err != nil {
		return err
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	return s.appendSagaStep(ctx, transferID, &pb.TransferCommitted{Id: transferID, Destinations: buildDestinations(legs)})
}

// cancelStaged submits a void_pending_transfer for every leg (releasing its
// TigerBeetle reservation), then cancels every DEBIT then every distinct
// CREDIT Operation (Cancel, not Fail — an external factor per decision #13),
// then appends TransferCancelled. Legal from either Staged or Pending:
// mechanically identical from either origin, since neither stage() nor
// confirmStaged() changes what's reserved in TigerBeetle.
//
// Claims CancellingStagedTransferStarted before touching TigerBeetle
// (go/docs/adr/0005): a client retry of CancelStagedTransfer, or a cancel
// racing PostPendingTransfer's commit() on the same Transfer, would
// otherwise both submit to TigerBeetle.
func (s *Server) cancelStaged(ctx context.Context, transferID, reason string) error {
	legs, preClaimState, err := s.loadLegs(ctx, transferID)
	if err != nil {
		return err
	}
	claimedSeq, won, err := s.claimForDispatch(ctx, transferID, preClaimState, &pb.CancellingStagedTransferStarted{Id: transferID, Reason: reason})
	if err != nil {
		return err
	}
	if !won {
		return nil
	}

	batch := make([]ledger.Transfer, len(legs))
	for i, leg := range legs {
		batch[i] = ledger.Transfer{
			ID:             detid.New(leg.GetDebitOperationId() + ":void"),
			DebitAccountID: leg.GetSourceTokenId(), CreditAccountID: leg.GetDestTokenId(),
			MinorUnits: leg.GetAmount().GetMinorUnits(), Currency: leg.GetAmount().GetCurrency(),
			Kind: ledger.TransferKindVoidPending, PendingID: leg.GetDebitOperationId(),
			Linked: i < len(legs)-1,
		}
	}
	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	if err := s.submitBatch(ctx, batch, func(legIndex int, result ledger.TransferResultCode) error {
		return twirp.InternalError(fmt.Sprintf("transfer %q: void leg %d rejected: %v", transferID, legIndex, result))
	}); err != nil {
		return err
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	if err := forEachOperation(legs, func(operationID string) error {
		_, err := operation.Cancel(ctx, s.store, operationID, reason)
		return err
	}); err != nil {
		return err
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	return s.appendSagaStep(ctx, transferID, &pb.TransferCancelled{Id: transferID, Reason: reason})
}

// compensate fails every DEBIT then every distinct CREDIT Operation (Fail,
// not Cancel — our own ledger's invariant, not an external factor, per
// decision #13), then appends TransferFailed. Called when TigerBeetle
// itself rejects a batch we submitted, at stage() or commit().
//
// No claim of its own (go/docs/adr/0005): compensate is only ever reached
// from inside stage()'s or commit()'s own onReject callback, after that
// caller already won the claim guarding the transition it's part of.
// claimedSeq is that caller's claim, re-checked here (requireClaim) since
// the CreateTransfers round trip that led here, and compensate's own
// Postgres writes, both take real time a reclaim could happen during —
// returning errClaimSuperseded propagates back through submitBatch's
// onReject to the caller's own switch, which treats it exactly like
// errBatchRejected.
func (s *Server) compensate(ctx context.Context, transferID string, claimedSeq int64, reason string) error {
	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		return err
	}
	legs, _, err := s.loadLegs(ctx, transferID)
	if err != nil {
		return err
	}

	if err := forEachOperation(legs, func(operationID string) error {
		_, err := operation.Fail(ctx, s.store, operationID, reason)
		return err
	}); err != nil {
		return err
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		return err
	}
	return s.appendSagaStep(ctx, transferID, &pb.TransferFailed{Id: transferID})
}

// cancelPrepared handles user-driven cancellation via CancelAcceptedTransfer
// — only legal while the Transfer is still Accepted or Prepared, before
// anything has been submitted to TigerBeetle. Unlike cancelStaged, no
// TigerBeetle call is ever needed here.
//
// The statePrepared branch claims CancellingPreparedTransferStarted before
// calling operation.Cancel (go/docs/adr/0005): a client retry of
// CancelAcceptedTransfer, or a cancel racing the orchestrator's automatic
// stage()/commit() dispatch from the same Prepared state, would otherwise
// both mutate every Operation. stateAccepted has no external side effect
// before its own append, so — like prepare() — it needs no claim.
func (s *Server) cancelPrepared(ctx context.Context, transferID, reason string) error {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return fmt.Errorf("transfer %q: not found", transferID)
	}

	switch state := currentState(events); state {
	case stateAccepted:
		return s.appendSagaStep(ctx, transferID, &pb.AcceptedTransferCancelled{Id: transferID, Reason: reason})
	case statePrepared:
		legs, err := preparedLegs(events, transferID)
		if err != nil {
			return err
		}
		claimedSeq, won, err := s.claimForDispatch(ctx, transferID, statePrepared, &pb.CancellingPreparedTransferStarted{Id: transferID, Reason: reason})
		if err != nil {
			return err
		}
		if !won {
			return nil
		}
		if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
			if errors.Is(err, errClaimSuperseded) {
				return nil
			}
			return err
		}
		if err := forEachOperation(legs, func(operationID string) error {
			_, err := operation.Cancel(ctx, s.store, operationID, reason)
			return err
		}); err != nil {
			return err
		}
		if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
			if errors.Is(err, errClaimSuperseded) {
				return nil
			}
			return err
		}
		return s.appendSagaStep(ctx, transferID, &pb.PreparedTransferCancelled{Id: transferID})
	case stateCommitted, stateFailed, stateCancelled:
		// Already resolved — idempotent no-op rather than an error.
		return nil
	default:
		return fmt.Errorf("transfer %q: cannot cancel from state %s", transferID, state)
	}
}

// Resume advances transferID's saga from whatever its stream currently
// records, and is the event-triggered orchestrator's entire vocabulary for a
// Transfer: a delivered message names an aggregate, and this re-folds that
// aggregate's authoritative state and dispatches whatever comes next
// (go/docs/adr/0001). It is exactly runSaga, exported — deliberately not a
// second implementation — so a trigger and an RPC drive the Transfer through
// the same code and converge on the same result when both do it at once.
//
// Safe to call any number of times: a Transfer already parked on the outside
// world or already terminal is left exactly as it is. An id with no stream is
// an error, not a no-op — every trigger names an aggregate the log already
// holds, so an unknown one is an inconsistency worth surfacing.
func (s *Server) Resume(ctx context.Context, transferID string) error {
	return s.runSaga(ctx, transferID)
}

// runSaga folds the Transfer's current state and dispatches the next step,
// looping until it reaches a state that waits on something outside this
// call — the outside world (Staged, Pending) or a true terminal (Committed,
// Failed, Cancelled). It is safe, and expected, to call this idempotently
// any number of times for the same id: each call resumes from wherever the
// stream actually left off (decision #5's crash-safety mitigation).
//
// A dispatch that returns nil without changing the folded state (statePrepared
// lost its claim to a concurrent caller — go/docs/adr/0005 — and stage()/
// commit() backed off) stops the loop rather than looping again: without
// this check, a claim loser would immediately reload, still see the
// pre-claim state (claim markers are invisible to currentState), and race to
// take a *fresh* claim on every iteration, turning the mutual exclusion the
// claim exists to provide into a busy-loop that still submits more than
// once.
func (s *Server) runSaga(ctx context.Context, transferID string) error {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return twirp.InternalErrorWith(err)
	}
	for {
		if len(events) == 0 {
			return fmt.Errorf("transfer %q: no events", transferID)
		}
		if rejectedRequest(events) {
			// A rejected request is terminal before the saga begins: there
			// are no legs, no Operations and no next step, so resuming one is
			// legitimately nothing to do. Ordinary business rejections reach
			// the orchestrator down the same topic as every other event
			// (go/docs/adr/0001), and treating one as an unrecognized state
			// would halt the consumer on a message no retry can get past.
			return nil
		}

		state := currentState(events)
		switch state {
		case stateAccepted:
			if err := s.prepare(ctx, transferID); err != nil {
				return err
			}
		case statePrepared:
			if reason, cancelling, err := claimedPreparedCancel(events); err != nil {
				return err
			} else if cancelling {
				if err := s.cancelPrepared(ctx, transferID, reason); err != nil {
					return err
				}
				break
			}
			requiresStaging, err := stageRequested(events)
			if err != nil {
				return err
			}
			if requiresStaging {
				if err := s.stage(ctx, transferID); err != nil {
					return err
				}
			} else if err := s.commit(ctx, transferID); err != nil {
				return err
			}
		case stateStaged, statePending:
			return nil
		case stateCommitted, stateFailed, stateCancelled:
			return nil
		default:
			return fmt.Errorf("transfer %q: saga stuck in unrecognized state", transferID)
		}

		events, err = s.store.Load(ctx, AggregateType, transferID)
		if err != nil {
			return twirp.InternalErrorWith(err)
		}
		if currentState(events) == state {
			return nil
		}
	}
}
