package transfer

import (
	"context"
	"errors"
	"fmt"

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

// currentState folds a Transfer's (or Reversal's — same aggregate type and
// saga shape) stream to find which state it's currently in, based on the
// last saga-outcome event recorded. This implementation emits exactly one
// event per saga transition (the outcome itself — TransferPrepared,
// TransferStaged, TransferCommitted, and so on) rather than every
// Start/*Started/Complete triplet the proto defines: nothing observes the
// in-between state in a synchronous, single-process saga (decision #5), so
// those intermediate markers would add event-log volume without changing
// behavior. The proto keeps them defined for a future async process manager
// that might want them.
func currentState(events []eventstore.Event) transferState {
	state := stateUnknown
	for _, e := range events {
		switch e.EventType {
		case eventstore.EventType(&pb.TransferRequestAccepted{}), eventstore.EventType(&pb.ReversalRequestAccepted{}):
			state = stateAccepted
		case eventstore.EventType(&pb.TransferPrepared{}):
			state = statePrepared
		case eventstore.EventType(&pb.TransferStaged{}):
			state = stateStaged
		case eventstore.EventType(&pb.TransferPending{}):
			state = statePending
		case eventstore.EventType(&pb.TransferCommitted{}):
			state = stateCommitted
		case eventstore.EventType(&pb.TransferFailed{}):
			state = stateFailed
		case eventstore.EventType(&pb.TransferCancelled{}),
			eventstore.EventType(&pb.AcceptedTransferCancelled{}),
			eventstore.EventType(&pb.PreparedTransferCancelled{}):
			state = stateCancelled
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
// A rejected request never advances past its first event, so it is checked
// for directly rather than through currentState, which has no vocabulary
// for it at all — a rejection is a terminal outcome unrelated to the saga's
// own accepted-through-committed progression.
func Outcome(ctx context.Context, store eventstore.Store, transferID string) (OutcomeKind, error) {
	events, err := store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return OutcomeNotFound, twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return OutcomeNotFound, nil
	}
	switch events[0].EventType {
	case eventstore.EventType(&pb.TransferRequestRejected{}), eventstore.EventType(&pb.ReversalRequestRejected{}):
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
// commit, cancelStaged, and compensate all start with.
func (s *Server) loadLegs(ctx context.Context, transferID string) ([]*pb.TransferLeg, error) {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	return preparedLegs(events, transferID)
}

// submitBatch submits batch to TigerBeetle and checks every result, calling
// onReject for the first leg that isn't OK or Exists — stage, commit, and
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
			return onReject(i, r.Result)
		}
	}
	return nil
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
func (s *Server) stage(ctx context.Context, transferID string) error {
	legs, err := s.loadLegs(ctx, transferID)
	if err != nil {
		return err
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
	if err := s.submitBatch(ctx, batch, func(legIndex int, result ledger.TransferResultCode) error {
		return s.compensate(ctx, transferID, fmt.Sprintf("tigerbeetle rejected staging leg %d: %v", legIndex, result))
	}); err != nil {
		return err
	}

	if err := forEachOperation(legs, func(operationID string) error {
		_, err := operation.Stage(ctx, s.store, operationID)
		return err
	}); err != nil {
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
func (s *Server) commit(ctx context.Context, transferID string) error {
	events, err := s.store.Load(ctx, AggregateType, transferID)
	if err != nil {
		return twirp.InternalErrorWith(err)
	}
	legs, err := preparedLegs(events, transferID)
	if err != nil {
		return err
	}
	// commit() needs the state itself (to pick immediate vs. posting mode),
	// which loadLegs doesn't expose — the one of the four callers that can't
	// use it.
	posting := currentState(events) == statePending

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
	if err := s.submitBatch(ctx, batch, func(legIndex int, result ledger.TransferResultCode) error {
		return s.compensate(ctx, transferID, fmt.Sprintf("tigerbeetle rejected commit leg %d: %v", legIndex, result))
	}); err != nil {
		return err
	}

	if err := forEachOperation(legs, func(operationID string) error {
		_, err := operation.Perform(ctx, s.store, operationID)
		return err
	}); err != nil {
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
func (s *Server) cancelStaged(ctx context.Context, transferID, reason string) error {
	legs, err := s.loadLegs(ctx, transferID)
	if err != nil {
		return err
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
	if err := s.submitBatch(ctx, batch, func(legIndex int, result ledger.TransferResultCode) error {
		return twirp.InternalError(fmt.Sprintf("transfer %q: void leg %d rejected: %v", transferID, legIndex, result))
	}); err != nil {
		return err
	}

	if err := forEachOperation(legs, func(operationID string) error {
		_, err := operation.Cancel(ctx, s.store, operationID, reason)
		return err
	}); err != nil {
		return err
	}

	return s.appendSagaStep(ctx, transferID, &pb.TransferCancelled{Id: transferID, Reason: reason})
}

// compensate fails every DEBIT then every distinct CREDIT Operation (Fail,
// not Cancel — our own ledger's invariant, not an external factor, per
// decision #13), then appends TransferFailed. Called when TigerBeetle
// itself rejects a batch we submitted, at stage() or commit().
func (s *Server) compensate(ctx context.Context, transferID, reason string) error {
	legs, err := s.loadLegs(ctx, transferID)
	if err != nil {
		return err
	}

	if err := forEachOperation(legs, func(operationID string) error {
		_, err := operation.Fail(ctx, s.store, operationID, reason)
		return err
	}); err != nil {
		return err
	}

	return s.appendSagaStep(ctx, transferID, &pb.TransferFailed{Id: transferID})
}

// cancelPrepared handles user-driven cancellation via CancelAcceptedTransfer
// — only legal while the Transfer is still Accepted or Prepared, before
// anything has been submitted to TigerBeetle. Unlike cancelStaged, no
// TigerBeetle call is ever needed here.
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
		if err := forEachOperation(legs, func(operationID string) error {
			_, err := operation.Cancel(ctx, s.store, operationID, reason)
			return err
		}); err != nil {
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
func (s *Server) runSaga(ctx context.Context, transferID string) error {
	for {
		events, err := s.store.Load(ctx, AggregateType, transferID)
		if err != nil {
			return twirp.InternalErrorWith(err)
		}
		if len(events) == 0 {
			return fmt.Errorf("transfer %q: no events", transferID)
		}

		switch state := currentState(events); state {
		case stateAccepted:
			if err := s.prepare(ctx, transferID); err != nil {
				return err
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
	}
}
