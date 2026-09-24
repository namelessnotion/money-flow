package transfer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"uuid"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/contention"
	"github.com/namelessnotion/money_flow/go/internal/detid"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
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
	eventstore.EventType(&pb.TransferRequestAccepted{}):   stateAccepted,
	eventstore.EventType(&pb.ReversalRequestAccepted{}):   stateAccepted,
	eventstore.EventType(&pb.TransferPrepared{}):          statePrepared,
	eventstore.EventType(&pb.TransferStaged{}):            stateStaged,
	eventstore.EventType(&pb.TransferPending{}):           statePending,
	eventstore.EventType(&pb.TransferCommitted{}):         stateCommitted,
	eventstore.EventType(&pb.TransferFailed{}):            stateFailed,
	eventstore.EventType(&pb.TransferCancelled{}):         stateCancelled,
	eventstore.EventType(&pb.AcceptedTransferCancelled{}): stateCancelled,
	eventstore.EventType(&pb.PreparedTransferCancelled{}): stateCancelled,
}

// transitions is a Transfer's lifecycle: for each state, the saga-outcome
// events that may be recorded next. appendSagaStep refuses anything else, so
// this is where "no leg reaches two different outcomes" is enforced — every
// leg of a Transfer reaches the Transfer's own outcome, together
// (go/docs/adr/0009). A refusal here is a saga contradiction and a bug, never
// a race: two callers racing the same step converge instead (appendSagaStep).
// The opening Accepted events, and TransferPrepared, are recorded by
// RequestTransfer/RequestReversal and prepare() under their own state checks,
// not through appendSagaStep, so they are not listed.
var transitions = map[transferState][]string{
	stateAccepted: {
		eventstore.EventType(&pb.AcceptedTransferCancelled{}),
	},
	statePrepared: {
		eventstore.EventType(&pb.TransferStaged{}),
		eventstore.EventType(&pb.TransferCommitted{}),
		eventstore.EventType(&pb.TransferFailed{}),
		eventstore.EventType(&pb.PreparedTransferCancelled{}),
	},
	stateStaged: {
		eventstore.EventType(&pb.TransferPending{}),
		eventstore.EventType(&pb.TransferCancelled{}),
	},
	statePending: {
		eventstore.EventType(&pb.TransferCommitted{}),
		eventstore.EventType(&pb.TransferFailed{}),
		eventstore.EventType(&pb.TransferCancelled{}),
	},
}

// TerminalEventTypes lists the event types after which a Transfer (or
// Reversal) never moves again: a rejected request, or any transition to
// committed, failed or cancelled. A reader of the whole log (cmd/resume -open)
// uses it to skip finished Transfers without folding each one. It is derived
// from stateByEventType, so the two cannot drift.
func TerminalEventTypes() []string {
	types := []string{
		eventstore.EventType(&pb.TransferRequestRejected{}),
		eventstore.EventType(&pb.ReversalRequestRejected{}),
	}
	for eventType, state := range stateByEventType {
		switch state {
		case stateCommitted, stateFailed, stateCancelled:
			types = append(types, eventType)
		}
	}
	sort.Strings(types)
	return types
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

// buildDestinations groups legs by destination Token, summing the amount
// each receives, for the TransferCommitted summary.
func buildDestinations(legs []*pb.TransferLeg) []*pb.TransferDestination {
	var order []string
	sums := make(map[string]uint64, len(legs))
	currency := ""
	for _, leg := range legs {
		destID := leg.GetDestTokenId()
		if _, seen := sums[destID]; !seen {
			order = append(order, destID)
		}
		sums[destID] += leg.GetAmount().GetMinorUnits()
		currency = leg.GetAmount().GetCurrency()
	}
	destinations := make([]*pb.TransferDestination, len(order))
	for i, destID := range order {
		destinations[i] = &pb.TransferDestination{
			ToTokenId: destID, Amount: &sharedpb.Money{MinorUnits: sums[destID], Currency: currency},
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
// through to recording an outcome after compensate() had just recorded
// TransferFailed, the exact contradiction appendSagaStep's transitions guard
// exists to catch.
var errBatchRejected = errors.New("transfer: batch rejected")

// legTransfers builds one ledger transfer per leg, in leg order — the order
// TransferPrepared recorded, so every retry cuts the same chains.
func legTransfers(legs []*pb.TransferLeg, build func(leg *pb.TransferLeg) ledger.Transfer) []ledger.Transfer {
	batch := make([]ledger.Transfer, len(legs))
	for i, leg := range legs {
		batch[i] = build(leg)
	}
	return batch
}

// reservations reserves every leg's amount as a TigerBeetle pending transfer
// under the leg's ledger transfer id — the id posts and voids refer back to.
func reservations(legs []*pb.TransferLeg) []ledger.Transfer {
	return legTransfers(legs, func(leg *pb.TransferLeg) ledger.Transfer {
		return ledger.Transfer{
			ID: leg.GetLedgerTransferId(), DebitAccountID: leg.GetSourceTokenId(), CreditAccountID: leg.GetDestTokenId(),
			MinorUnits: leg.GetAmount().GetMinorUnits(), Currency: leg.GetAmount().GetCurrency(),
			Kind: ledger.TransferKindPending, Timeout: stagingTimeoutSeconds,
		}
	})
}

// settlements finalizes (kind PostPending) or releases (kind VoidPending)
// every leg's reservation, under an id derived from the leg's ledger transfer
// id and suffix so a retry answers Exists.
func settlements(legs []*pb.TransferLeg, kind ledger.TransferKind, suffix string) []ledger.Transfer {
	return legTransfers(legs, func(leg *pb.TransferLeg) ledger.Transfer {
		return ledger.Transfer{
			ID:             detid.New(leg.GetLedgerTransferId() + suffix),
			DebitAccountID: leg.GetSourceTokenId(), CreditAccountID: leg.GetDestTokenId(),
			MinorUnits: leg.GetAmount().GetMinorUnits(), Currency: leg.GetAmount().GetCurrency(),
			Kind: kind, PendingID: leg.GetLedgerTransferId(),
		}
	})
}

// chains cuts batch into consecutive linked chains of at most
// ledger.BatchMax legs — the most one TigerBeetle request carries, and so the
// most TigerBeetle can apply all-or-nothing (go/docs/adr/0008). A Transfer
// narrower than that is one chain, exactly as before the cap existed.
func chains(batch []ledger.Transfer) [][]ledger.Transfer {
	var out [][]ledger.Transfer
	for start := 0; start < len(batch); start += ledger.BatchMax {
		chain := batch[start:min(start+ledger.BatchMax, len(batch))]
		for i := range chain {
			chain[i].Linked = i < len(chain)-1
		}
		out = append(out, chain)
	}
	return out
}

// ledgerRefusal is TigerBeetle refusing one chain of a saga step's batch.
type ledgerRefusal struct {
	reason string
	// chainsApplied counts the chains before the refused one, all of which
	// TigerBeetle applied. Always zero for a Transfer that fits one chain;
	// for a wider one it decides whether Failed can still be the truth.
	chainsApplied int
}

// submitBatch submits batch to TigerBeetle one linked chain per request, in
// order, stopping at the first chain TigerBeetle refuses — a leg that isn't
// OK or Exists, or the request as a whole refused as ledger.ErrInvalidRequest,
// which no retry can change — and handing that refusal to onReject. It
// records no balances when every chain is accepted: the step that submitted
// the batch records them with its own outcome (recordOutcome). stage, commit,
// and cancelStaged all
// submit a batch this same way and only differ in what "refused" means for
// them (stage/commit route it to compensate() as an internal-invariant
// Failed; cancelStaged treats it as a plain internal error, since a void
// being rejected isn't decision #13's Cancelled/Failed split at all — see
// cancelStaged's own comment).
//
// Stopping at the first refusal is what keeps a wide post from landing
// piecemeal: chains are reserved in this same order, so the first chain is
// the first to lapse, and a post refused anywhere but the first chain means a
// reservation disappeared out from under a live claim (go/docs/adr/0008).
func (s *Server) submitBatch(
	ctx context.Context, batch []ledger.Transfer, onReject func(ledgerRefusal) error,
) error {
	for c, chain := range chains(batch) {
		refusal, err := s.submitChain(ctx, chain, c)
		if err != nil {
			return err
		}
		if refusal == nil {
			continue
		}
		if c > 0 {
			// The chains before this one moved balances; publish them before
			// deciding anything, so the read side never lags the ledger.
			if err := s.recordTouched(ctx, batch[:c*ledger.BatchMax]); err != nil {
				return err
			}
		}
		if err := onReject(*refusal); err != nil {
			return err
		}
		return errBatchRejected
	}
	return nil
}

// submitChain submits chain, the c'th of its batch, returning the refusal if
// TigerBeetle refused it. Any other error is left to the saga's driver to
// retry.
func (s *Server) submitChain(ctx context.Context, chain []ledger.Transfer, c int) (*ledgerRefusal, error) {
	results, err := s.ledger.CreateTransfers(ctx, chain)
	switch {
	case errors.Is(err, ledger.ErrInvalidRequest):
		return &ledgerRefusal{reason: fmt.Sprintf("ledger refused chain %d: %v", c, err), chainsApplied: c}, nil
	case err != nil:
		return nil, twirp.InternalErrorWith(fmt.Errorf("ledger: %w", err))
	}
	for i, r := range results {
		if r.Result != ledger.TransferResultOK && r.Result != ledger.TransferResultExists {
			return &ledgerRefusal{
				reason: fmt.Sprintf("leg %d: %v", c*ledger.BatchMax+i, r.Result), chainsApplied: c,
			}, nil
		}
	}
	return nil, nil
}

// recordTouched records the balance of every Token batch touched, one append
// per Token — only for chains a refusal left applied, which no outcome of
// their own will carry.
func (s *Server) recordTouched(ctx context.Context, batch []ledger.Transfer) error {
	touched := make([]string, 0, 2*len(batch))
	for _, t := range batch {
		touched = append(touched, t.DebitAccountID, t.CreditAccountID)
	}
	return token.RecordBalances(ctx, s.store, s.ledger, touched)
}

// appendSagaStep records event, a saga step's outcome with no ledger write
// behind it, as the next fact on transferID's own stream (recordOutcome).
func (s *Server) appendSagaStep(ctx context.Context, transferID string, event proto.Message) error {
	return s.recordOutcome(ctx, transferID, event, nil)
}

// recordOutcome records event, a saga step's outcome, as the next fact on
// transferID's own stream, together with the balance of every Token legs
// name, in one atomic write (go/docs/adr/0010). The balances are what the
// step's ledger write just did to those Tokens: recording them with the
// outcome means the read side learns both at once, and a crash before the
// write loses neither — the step is retried, TigerBeetle answers Exists, and
// both are built again.
//
// It converges when event is already the Transfer's latest outcome — a
// retried saga step, or another concurrent call driving the same Transfer,
// already recorded it, perhaps with a client's *Rejected response landing on
// top since — and refuses, as an internal error, any outcome the Transfer's
// lifecycle (transitions) does not allow from where it stands.
//
// Losing the append is contention, not a fault: another write landed on the
// Transfer's stream or on one of its Tokens' — a busy source Token is shared
// by every Transfer debiting it. It rebuilds against what landed and tries
// again, for as long as it keeps losing to something (go/docs/adr/0003).
func (s *Server) recordOutcome(ctx context.Context, transferID string, event proto.Message, legs []*pb.TransferLeg) error {
	wantType := eventstore.EventType(event)
	for attempt := 0; ; attempt++ {
		events, err := s.store.Load(ctx, AggregateType, transferID)
		if err != nil {
			return twirp.InternalErrorWith(err)
		}
		if latest, ok := latestOutcome(events); ok && latest.EventType == wantType {
			return nil
		}
		if state := currentState(events); !slices.Contains(transitions[state], wantType) {
			return twirp.InternalError(fmt.Sprintf(
				"transfer %q: already %s, cannot also become %s", transferID, state, wantType))
		}

		writes, err := token.BalanceWrites(ctx, s.store, s.ledger, legTokens(legs))
		if err != nil {
			return err
		}
		writes = append(writes, eventstore.StreamWrite{
			AggregateType: AggregateType, AggregateID: transferID, ExpectedSeq: int64(len(events)),
			Events: []proto.Message{event},
		})
		switch err := s.store.AppendAtomic(ctx, writes...); {
		case err == nil:
			return nil
		case !errors.Is(err, eventstore.ErrConcurrencyConflict):
			return twirp.InternalErrorWith(err)
		}
		switch overtaken, err := s.overtaken(ctx, writes); {
		case err != nil:
			return err
		case !overtaken:
			return twirp.InternalError(fmt.Sprintf(
				"transfer %q: recording %s conflicted, yet no stream it wrote to has moved", transferID, wantType))
		}
		if err := contention.Wait(ctx, attempt); err != nil {
			return err
		}
	}
}

// legTokens is every Token legs move money between.
func legTokens(legs []*pb.TransferLeg) []string {
	ids := make([]string, 0, 2*len(legs))
	for _, leg := range legs {
		ids = append(ids, leg.GetSourceTokenId(), leg.GetDestTokenId())
	}
	return ids
}

// latestOutcome is the last event on events that advances currentState()'s
// fold, skipping claim markers and *Rejected responses.
func latestOutcome(events []eventstore.Event) (eventstore.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if _, advances := stateByEventType[events[i].EventType]; advances {
			return events[i], true
		}
	}
	return eventstore.Event{}, false
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

// isClaimMarkerType reports whether eventType is one of the three claim
// markers stage()/commit()/cancelStaged() append — the steps with a
// TigerBeetle side effect to guard. CancellingPreparedTransferStarted is
// deliberately absent: cancelPrepared() stopped claiming (go/docs/adr/0009),
// and one an older build left on a stream guards nothing that outlived it.
func isClaimMarkerType(eventType string) bool {
	switch eventType {
	case eventstore.EventType(&pb.StagingTransferStarted{}),
		eventstore.EventType(&pb.TransferCommittingStarted{}),
		eventstore.EventType(&pb.CancellingStagedTransferStarted{}):
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
// with the still-in-flight first one against the same legs, reproducing
// the exact contradiction this decision exists to prevent.
//
// A marker older than claimStaleAfter is treated as abandoned and is safe
// to re-claim with the same marker type — this is what preserves crash
// recovery without a lease or a heartbeat: currentState() still ignores
// every marker, so re-claiming just re-enters the same, already-idempotent
// dispatch path. A different transition is refused (errAbandonedClaim)
// rather than let through: "abandoned" is a guess, the old winner may be
// merely slow and mid-step, and even if it did crash it may have
// half-applied its step (some chains voided in TigerBeetle, not yet all)
// — the same transition converges over that, a different one contradicts
// it. The orchestrator never trips this refusal on its own dispatch: from
// Prepared it only ever stages or commits, whichever the Transfer's stage
// flag says, never both. Staleness is
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
// compensate() immediately before each externally visible
// action, stops the slow winner at its next step. It cannot stop it mid-step
// (inside a TigerBeetle round trip, or a wide batch's run of chains), which
// is why only the same marker type may take a stale claim over: that overlap
// converges on idempotent writes, a different transition's would not.
//
// won=false, err=nil means the aggregate already moved past preClaimState —
// by this call's own eventual claim below, or a concurrent one — and the
// caller has nothing left to do; it should return nil unconditionally,
// never retry a fresh claim itself. On won=true, claimedSeq is the
// sequence number the marker landed at, for stillHoldsClaim.
func (s *Server) claimForDispatch(ctx context.Context, transferID string, preClaimState transferState, marker proto.Message) (claimedSeq int64, won bool, err error) {
	for {
		events, moved, err := s.awaitUnclaimed(ctx, transferID, preClaimState, marker)
		if err != nil || moved {
			return 0, false, err
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

// awaitUnclaimed waits until transferID has no fresh live claim, and returns
// its stream as it then stands — the wait half of claimForDispatch, shared
// with cancelPrepared(), which has no side effect of its own to claim but
// must still not land while another step's is in flight. moved=true means
// the Transfer left preClaimState while waiting (or already had), and the
// caller has nothing left to do.
//
// A stale marker may be taken over only by the transition that wrote it:
// next is the caller's own marker, or its outcome event when it claims
// nothing, which never matches a marker and so never takes one over
// (errAbandonedClaim).
func (s *Server) awaitUnclaimed(ctx context.Context, transferID string, preClaimState transferState, next proto.Message) (events []eventstore.Event, moved bool, err error) {
	for {
		events, err := s.store.Load(ctx, AggregateType, transferID)
		if err != nil {
			return nil, false, twirp.InternalErrorWith(err)
		}
		if currentState(events) != preClaimState {
			return nil, true, nil
		}
		live, ok := liveMarker(events)
		if !ok {
			return events, false, nil
		}
		now, err := s.store.Now(ctx)
		if err != nil {
			return nil, false, twirp.InternalErrorWith(err)
		}
		if now.Sub(live.OccurredAt) >= claimStaleAfter {
			if live.EventType != eventstore.EventType(next) {
				return nil, false, abandonedClaimError(transferID, live.EventType, next)
			}
			return events, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, twirp.InternalErrorWith(ctx.Err())
		case <-time.After(claimPollInterval):
		}
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

// stillHoldsClaim reports whether transferID's live marker (liveMarker) is
// still the one this caller appended at claimedSeq — called immediately
// before each externally visible action in stage()/commit()/cancelStaged()/
// compensate() (go/docs/adr/0005: a claim can be
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
// compensate() needs: nil means still held, proceed;
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

// errPlanOvertaken is tryPrepare losing its append to a write that landed on
// a stream it planned against — a Wallet it mints into, or the Transfer's own
// stream: the plan was built on a position that no longer holds.
var errPlanOvertaken = errors.New("plan overtaken by a write to a stream it planned against")

// prepare mints the destination Token(s) (skipped for a reversal — its
// destinations are always the original Transfer's own, pre-existing source
// Tokens) and records every leg on TransferPrepared, all in one AppendAtomic
// — the Transfer's legs and any new Token stream(s) are *created together*,
// the same shape as Holder.Provision.
//
// Minting appends to the Wallet at the position it was loaded at, so
// Transfers minting into one Wallet — a shared destination, or a reserve
// every mint_source Transfer mints its source from — collide there when they
// prepare at once. That is contention, not a fault: the loser re-plans
// against the Wallet as it now stands, reloading everything, exactly as a
// fresh prepare would. It keeps re-planning for as long as it keeps losing,
// because every loss is some other write landing, so a hot Wallet drains
// rather than halting the orchestrator (go/docs/adr/0003, amended
// 2026-09-24). No fixed number of re-plans is enough: with k Transfers
// preparing against one Wallet at once, the last to land loses k-1 times,
// and k grows with the orchestrator's partition count.
//
// The same write claims the step that follows — stage() or commit(), by the
// request's stage flag — and prepare() returns that claim for runSaga to go
// straight on with (claimedStep). It returns nil when there was nothing left
// to prepare.
//
// Two things end the loop other than landing: ctx, and tryPrepare's check
// that a lost race was lost *to* something. A conflict nothing landed to
// cause is a fault, and goes back to the driver to retry and halt over.
func (s *Server) prepare(ctx context.Context, transferID string) (*claimedStep, error) {
	for attempt := 0; ; attempt++ {
		next, err := s.tryPrepare(ctx, transferID)
		if !errors.Is(err, errPlanOvertaken) {
			return next, err
		}
		if err := contention.Wait(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

// claimedStep is the step prepare() claimed for the Transfer it prepared:
// stage() when the request asked for staging, commit() otherwise, with the
// claim at seq and the legs it recorded.
type claimedStep struct {
	seq   int64
	legs  []*pb.TransferLeg
	stage bool
}

// tryPrepare is one planning of prepare against the store as it stands now.
// It reports errPlanOvertaken when its write lost to a different one.
func (s *Server) tryPrepare(ctx context.Context, transferID string) (*claimedStep, error) {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("transfer %q: no accepted event to prepare from", transferID)
	}
	if currentState(events) != stateAccepted {
		// Overtaken on the Transfer's own stream: a concurrent prepare of
		// this Transfer landed, or a cancel did. Either way there is nothing
		// left to prepare, and planning anyway would append TransferPrepared
		// after the cancel — a cancelled Transfer brought back to move money.
		// runSaga reloads and carries on from wherever it now is.
		return nil, nil
	}
	msg, err := events[0].Decode()
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
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
				return nil, err
			}
			if rejection != nil {
				return nil, fmt.Errorf("transfer %q: prepare: mint_source re-validation failed after accept: %s", transferID, rejection.GetReason())
			}

			srcSpec := mintSourceLeg(transferID, accepted.GetAmount())
			srcWalletEvents, err := s.store.Load(ctx, wallet.AggregateType, accepted.GetFromWalletId())
			if err != nil {
				return nil, twirp.InternalErrorWith(err)
			}
			writes, mintRejection, err := token.MintWrites(
				ctx, s.store, s.ledger, accepted.GetFromWalletId(), srcWalletEvents,
				[]token.MintSpec{srcSpec}, accepted.GetTransactionId(),
			)
			if err != nil {
				return nil, err
			}
			if mintRejection != nil {
				return nil, fmt.Errorf("transfer %q: prepare: source mint rejected: %s", transferID, mintRejection.GetReason())
			}
			srcMintWrites = writes
			srcLegs = []Leg{{SourceTokenID: srcSpec.TokenID, Amount: accepted.GetAmount()}}
		} else {
			selected, rejection, err := selectSourceTokens(
				ctx, s.store, s.ledger, accepted.GetFromWalletId(), accepted.GetAmount(),
				accepted.GetTransactionId(), s.isOpen,
			)
			if err != nil {
				return nil, err
			}
			if rejection != nil {
				return nil, fmt.Errorf("transfer %q: prepare: re-selection failed after accept: %s", transferID, rejection.GetReason())
			}
			srcLegs = selected
		}

		destSpecs := planDestinations(transferID, accepted.GetAmount())
		walletEvents, err := s.store.Load(ctx, wallet.AggregateType, accepted.GetToWalletId())
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		writes, mintRejection, err := token.MintWrites(
			ctx, s.store, s.ledger, accepted.GetToWalletId(), walletEvents, destSpecs, accepted.GetTransactionId(),
		)
		if err != nil {
			return nil, err
		}
		if mintRejection != nil {
			return nil, fmt.Errorf("transfer %q: prepare: mint rejected: %s", transferID, mintRejection.GetReason())
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
			return nil, err
		}
		if rejection != nil {
			return nil, fmt.Errorf("transfer %q: prepare: reversal manifest failed after accept: %s", transferID, rejection.GetReason())
		}
		legs = revLegs

	default:
		return nil, fmt.Errorf("transfer %q: stream starts with %s, want an Accepted event", transferID, events[0].EventType)
	}

	// Each leg's ledger transfer id is generated per planning: only the plan
	// that lands is ever submitted, and every later step reads its ids back
	// off TransferPrepared.
	protoLegs := make([]*pb.TransferLeg, len(legs))
	for i, leg := range legs {
		protoLegs[i] = &pb.TransferLeg{
			SourceTokenId: leg.SourceTokenID, DestTokenId: leg.DestTokenID, Amount: leg.Amount,
			LedgerTransferId: uuid.NewV7().String(),
		}
	}

	// The Transfer is prepared and its next step claimed in one write: the
	// saga goes straight on to that step, so a separate claim would only cost
	// another commit (go/docs/adr/0010). The claim sits right after
	// TransferPrepared.
	stage, err := stageRequested(events)
	if err != nil {
		return nil, err
	}
	var marker proto.Message = &pb.TransferCommittingStarted{Id: transferID}
	if stage {
		marker = &pb.StagingTransferStarted{Id: transferID}
	}
	next := &claimedStep{seq: int64(len(events)) + 2, legs: protoLegs, stage: stage}

	writes := make([]eventstore.StreamWrite, 0, len(mintWrites)+1)
	writes = append(writes, mintWrites...)
	writes = append(writes, eventstore.StreamWrite{
		AggregateType: AggregateType, AggregateID: transferID, ExpectedSeq: int64(len(events)),
		Events: []proto.Message{&pb.TransferPrepared{Id: transferID, Legs: protoLegs}, marker},
	})

	switch err := s.store.AppendAtomic(ctx, writes...); {
	case err == nil:
		return next, nil
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		switch overtaken, loadErr := s.overtaken(ctx, writes); {
		case loadErr != nil:
			return nil, loadErr
		case !overtaken:
			return nil, fmt.Errorf("transfer %q: prepare: %w, yet no stream it planned against has moved", transferID, err)
		}
		return nil, errPlanOvertaken
	default:
		return nil, twirp.InternalErrorWith(err)
	}
}

// overtaken reports whether any stream writes expected at a position has
// since moved past it: whether an append that lost did so to a write that
// really landed, which is what makes re-planning worth it. Only streams that
// already existed are checked. Every stream a plan creates — its Tokens — is
// created in the same AppendAtomic as the Transfer's own TransferPrepared, so
// anything that created one first moved the Transfer's stream too.
func (s *Server) overtaken(ctx context.Context, writes []eventstore.StreamWrite) (bool, error) {
	for _, w := range writes {
		if w.ExpectedSeq == 0 {
			continue
		}
		events, err := s.store.Load(ctx, w.AggregateType, w.AggregateID)
		if err != nil {
			return false, twirp.InternalErrorWith(err)
		}
		if int64(len(events)) != w.ExpectedSeq {
			return true, nil
		}
	}
	return false, nil
}

// stage submits every leg as a TigerBeetle pending transfer (reserving
// capacity, posting nothing), then appends TransferStaged. A TigerBeetle-
// level rejection here is our own ledger's invariant failing, so it routes
// to compensate() (Failed), not cancelStaged() (Cancelled) — see decision
// #13.
//
// Claims StagingTransferStarted before touching TigerBeetle (go/docs/adr/0005):
// two drivers of the same Transfer — a redelivered trigger handled beside the
// original, or cmd/resume running alongside cmd/orchestrator — can both reach
// here from the same statePrepared read, with nothing else stopping them from
// both submitting to TigerBeetle at once. A caller that loses the claim
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
	return s.stageClaimed(ctx, transferID, claimedSeq, legs)
}

// stageClaimed is stage() once its claim is held at claimedSeq — taken by
// stage() itself, or by prepare() in the same write as TransferPrepared.
func (s *Server) stageClaimed(ctx context.Context, transferID string, claimedSeq int64, legs []*pb.TransferLeg) error {
	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	// A refusal after earlier chains reserved still fails the Transfer:
	// nothing is posted, and those reservations lapse at their timeout
	// (go/docs/adr/0008 says why they are not voided here).
	switch err := s.submitBatch(ctx, reservations(legs), func(r ledgerRefusal) error {
		return s.compensate(ctx, transferID, claimedSeq, "tigerbeetle rejected staging "+r.reason)
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
	return s.recordOutcome(ctx, transferID, &pb.TransferStaged{Id: transferID}, legs)
}

// confirmStaged records TransferPending — a pure event-log write, called
// only from ConfirmStagedTransfer. No TigerBeetle call: the reservation
// already exists from stage().
func (s *Server) confirmStaged(ctx context.Context, transferID string) error {
	return s.appendSagaStep(ctx, transferID, &pb.TransferPending{Id: transferID})
}

// commit moves every leg in TigerBeetle (moveMoney), then appends
// TransferCommitted. From Prepared (the immediate, non-staged path) this is
// a fresh transfer batch — reserved first, then posted, if it spans more
// than one ledger chain (go/docs/adr/0008); from Pending (called via
// PostPendingTransfer) it's a post_pending_transfer batch referencing each
// leg's already-staged TigerBeetle transfer — "posted" and "committed" are
// the same terminal state reached by two different routes. A TigerBeetle-
// level rejection with nothing yet posted routes to compensate() (Failed)
// either way.
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
	// doesn't expose — the one of the four callers that can't use it. A
	// caller that reaches here after the Transfer moved on — it lost the
	// race to a concurrent commit, or to a cancel — has nothing to do: taking
	// the state it finds as preClaimState would claim a finished Transfer.
	preClaimState := currentState(events)
	if preClaimState != statePrepared && preClaimState != statePending {
		return nil
	}
	posting := preClaimState == statePending

	claimedSeq, won, err := s.claimForDispatch(ctx, transferID, preClaimState, &pb.TransferCommittingStarted{Id: transferID})
	if err != nil {
		return err
	}
	if !won {
		return nil
	}
	return s.commitClaimed(ctx, transferID, claimedSeq, legs, posting)
}

// commitClaimed is commit() once its claim is held at claimedSeq — taken by
// commit() itself, or, for an immediate Transfer, by prepare() in the same
// write as TransferPrepared.
func (s *Server) commitClaimed(ctx context.Context, transferID string, claimedSeq int64, legs []*pb.TransferLeg, posting bool) error {
	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	switch done, err := s.moveMoney(ctx, transferID, claimedSeq, legs, posting); {
	case err != nil:
		return err
	case done:
		return nil // compensate() recorded TransferFailed, or found itself superseded
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	return s.recordOutcome(ctx, transferID, &pb.TransferCommitted{Id: transferID, Destinations: buildDestinations(legs)}, legs)
}

// moveMoney is commit()'s TigerBeetle half. done=true means a refusal was
// handled — compensate() recorded TransferFailed, or found its claim
// superseded — and commit() has nothing left to do.
//
// Posting from Pending finalizes the reservations stage() made. From Prepared,
// a Transfer that fits one chain posts directly, since TigerBeetle applies one
// chain all-or-nothing. A wider one cannot be posted all-or-nothing by
// anything TigerBeetle offers, so it is reserved first — a refusal there posts
// nothing, and fails the Transfer as any refused leg does — and only then
// posted, which nothing but a lapsed or voided reservation can refuse
// (go/docs/adr/0008).
func (s *Server) moveMoney(ctx context.Context, transferID string, claimedSeq int64, legs []*pb.TransferLeg, posting bool) (done bool, err error) {
	switch {
	case posting:
		return s.settle(ctx, transferID, claimedSeq, settlements(legs, ledger.TransferKindPostPending, ":post"))
	case len(legs) <= ledger.BatchMax:
		return s.settle(ctx, transferID, claimedSeq, legTransfers(legs, func(leg *pb.TransferLeg) ledger.Transfer {
			return ledger.Transfer{
				ID: leg.GetLedgerTransferId(), DebitAccountID: leg.GetSourceTokenId(), CreditAccountID: leg.GetDestTokenId(),
				MinorUnits: leg.GetAmount().GetMinorUnits(), Currency: leg.GetAmount().GetCurrency(),
				Kind: ledger.TransferKindRegular,
			}
		}))
	}

	switch err := s.submitBatch(ctx, reservations(legs), func(r ledgerRefusal) error {
		return s.compensate(ctx, transferID, claimedSeq, "tigerbeetle rejected reserving "+r.reason)
	}); {
	case err == nil:
	case errors.Is(err, errBatchRejected), errors.Is(err, errClaimSuperseded):
		return true, nil
	default:
		return false, err
	}
	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return true, nil
		}
		return false, err
	}
	return s.settle(ctx, transferID, claimedSeq, settlements(legs, ledger.TransferKindPostPending, ":post"))
}

// settle submits batch, the one that moves money. A refusal with nothing yet
// posted fails the Transfer. One after an earlier chain posted cannot: part of
// the money has moved, so neither Failed nor Committed is true, and it is
// returned as an error for the orchestrator to halt on (go/docs/adr/0003) and
// a person to reconcile.
func (s *Server) settle(ctx context.Context, transferID string, claimedSeq int64, batch []ledger.Transfer) (done bool, err error) {
	switch err := s.submitBatch(ctx, batch, func(r ledgerRefusal) error {
		if r.chainsApplied > 0 {
			return twirp.InternalError(fmt.Sprintf(
				"transfer %q: tigerbeetle rejected commit %s after %d chain(s) of %d legs had posted; partially posted, needs reconciling (go/docs/adr/0008)",
				transferID, r.reason, r.chainsApplied, ledger.BatchMax,
			))
		}
		return s.compensate(ctx, transferID, claimedSeq, "tigerbeetle rejected commit "+r.reason)
	}); {
	case err == nil:
		return false, nil
	case errors.Is(err, errBatchRejected), errors.Is(err, errClaimSuperseded):
		return true, nil
	default:
		return false, err
	}
}

// cancelStaged submits a void_pending_transfer for every leg (releasing its
// TigerBeetle reservation), then appends TransferCancelled (not
// TransferFailed — an external factor per decision #13). Legal from either
// Staged or Pending: mechanically identical from either origin, since
// neither stage() nor confirmStaged() changes what's reserved in
// TigerBeetle.
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
	if preClaimState != stateStaged && preClaimState != statePending {
		// Moved on since the caller looked — most often a concurrent cancel
		// that landed first. Nothing left to claim.
		return nil
	}
	claimedSeq, won, err := s.claimForDispatch(ctx, transferID, preClaimState, &pb.CancellingStagedTransferStarted{Id: transferID, Reason: reason})
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
	if err := s.submitBatch(ctx, settlements(legs, ledger.TransferKindVoidPending, ":void"), func(r ledgerRefusal) error {
		return twirp.InternalError(fmt.Sprintf("transfer %q: void rejected: %s", transferID, r.reason))
	}); err != nil {
		return err
	}

	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		if errors.Is(err, errClaimSuperseded) {
			return nil
		}
		return err
	}
	return s.recordOutcome(ctx, transferID, &pb.TransferCancelled{Id: transferID, Reason: reason}, legs)
}

// compensate appends TransferFailed, carrying reason — our own ledger's
// invariant refused a batch we submitted, at stage() or commit(), which is
// not an external factor, so Failed rather than Cancelled (decision #13).
//
// No claim of its own (go/docs/adr/0005): compensate is only ever reached
// from inside stage()'s or commit()'s own onReject callback, after that
// caller already won the claim guarding the transition it's part of.
// claimedSeq is that caller's claim, re-checked here (requireClaim) since
// the CreateTransfers round trip that led here takes real time a reclaim
// could happen during — returning errClaimSuperseded propagates back
// through submitBatch's onReject to the caller's own switch, which treats
// it exactly like errBatchRejected.
func (s *Server) compensate(ctx context.Context, transferID string, claimedSeq int64, reason string) error {
	if err := s.requireClaim(ctx, transferID, claimedSeq); err != nil {
		return err
	}
	return s.appendSagaStep(ctx, transferID, &pb.TransferFailed{Id: transferID, Reason: reason})
}

// cancelPrepared handles user-driven cancellation via CancelAcceptedTransfer
// — only legal while the Transfer is still Accepted or Prepared, before
// anything has been submitted to TigerBeetle. Unlike cancelStaged, no
// TigerBeetle call is ever needed here, so it claims nothing
// (go/docs/adr/0009): the cancel is one append, landing on the stream
// exactly as it was loaded. That append is the compare-and-swap against
// every other writer. If a prepare() or a stage()/commit() claim lands
// first, the append loses and the caller re-decides; if the cancel lands
// first, their claim loses and they find the Transfer cancelled.
//
// From Prepared it first waits out any fresh claim (awaitUnclaimed): a
// stage() or commit() already submitting to TigerBeetle must finish, never
// be overtaken by a cancel recorded underneath it. It refuses over an
// abandoned one (errAbandonedClaim) — only that transition may resume it.
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
		_, err := s.tryAppend(ctx, transferID, int64(len(events)), &pb.AcceptedTransferCancelled{Id: transferID, Reason: reason})
		return err
	case statePrepared:
		cancelled := &pb.PreparedTransferCancelled{Id: transferID, Reason: reason}
		for {
			events, moved, err := s.awaitUnclaimed(ctx, transferID, statePrepared, cancelled)
			if err != nil || moved {
				return err
			}
			// A lost append means some other write landed; wait out
			// whatever it was and re-decide.
			if landed, err := s.tryAppend(ctx, transferID, int64(len(events)), cancelled); err != nil || landed {
				return err
			}
		}
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
// second implementation — and since the async cutover (go/docs/adr/0006) it is
// the only way a Transfer's saga runs: no RPC drives one. Two concurrent calls
// for the same id (a redelivery, or cmd/resume beside the orchestrator)
// converge on the same result.
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
			// are no legs and no next step, so resuming one is
			// legitimately nothing to do. Ordinary business rejections reach
			// the orchestrator down the same topic as every other event
			// (go/docs/adr/0001), and treating one as an unrecognized state
			// would halt the consumer on a message no retry can get past.
			return nil
		}

		state := currentState(events)
		switch state {
		case stateAccepted:
			next, err := s.prepare(ctx, transferID)
			if err != nil {
				return err
			}
			// Nil when prepare() found nothing left to prepare; the reload
			// below carries on from wherever the Transfer now is.
			if next != nil {
				if next.stage {
					err = s.stageClaimed(ctx, transferID, next.seq, next.legs)
				} else {
					err = s.commitClaimed(ctx, transferID, next.seq, next.legs, false)
				}
				if err != nil {
					return err
				}
			}
		case statePrepared:
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
