// Package transfer implements the Transfer root aggregate: moving value
// between Wallets by debiting existing source Token(s) and crediting
// newly-minted destination Token(s), including a reversal flow and a
// staged/pending settlement path for external rails like ACH. See the
// implementation plan for the full saga design.
//
// RequestTransfer and RequestReversal record a decision and return; the saga
// itself runs in cmd/orchestrator, from the events those decisions publish.
// The settlement RPCs below (ConfirmStagedTransfer, PostPendingTransfer,
// CancelStagedTransfer, CancelAcceptedTransfer) are the exception, and are not
// really one: each performs a single claimed transition for one Transfer and
// answers with its outcome, which is what its caller needs. None of them
// folds.
package transfer

import (
	"context"
	"errors"
	"fmt"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/id"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/money"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

var _ pb.TransferService = (*Server)(nil)

// Server implements the Transfer Twirp service. Minting and committing
// unavoidably cross into TigerBeetle, so like token.Server it holds a
// ledger.Client.
//
// isOpen and transactionExists are both nil-safe (isOpen: wallet's own
// TokensOfVisibleTo degrades to unfiltered TokensOf behavior on a nil
// checker; transactionExists: mint_source itself already defaults false, so
// it is simply never consulted) — every existing call site can keep passing
// nil for both.
type Server struct {
	store             eventstore.Store
	ledger            ledger.Client
	isOpen            wallet.TransactionOpenChecker
	transactionExists TransactionExistsChecker
}

func NewServer(store eventstore.Store, lc ledger.Client, isOpen wallet.TransactionOpenChecker, transactionExists TransactionExistsChecker) *Server {
	return &Server{store: store, ledger: lc, isOpen: isOpen, transactionExists: transactionExists}
}

// maxConcurrencyAttempts bounds every "load, decide, try to append" loop
// below. Each attempt reloads current state, so a conflict never blindly
// retries the same decision — it re-derives it from what's actually there
// now, converging on any outcome a concurrent identical call already wrote,
// or re-evaluating from scratch if the state moved on. Exhausting every
// attempt means something is persistently racing this same id; that's
// reported as a retryable error (twirp.Aborted) rather than silently
// dropped or treated as a domain rejection.
const maxConcurrencyAttempts = 3

func abortedRetry(id string) error {
	return twirp.NewError(twirp.Aborted, fmt.Sprintf(
		"transfer %q: could not converge after repeated concurrency conflicts; retry", id,
	))
}

// tryAppend appends event to transferID's stream at expectedSeq. It reports
// ok=false, not an error, when the append lost a race against a concurrent
// write to this same stream — callers should reload fresh state and
// re-decide from scratch (see maxConcurrencyAttempts), since what actually
// landed might change what should happen now: the state may have advanced
// past what was true a moment ago, or an identical write from another
// caller may have already landed. This is the one place every RPC handler
// below decides whether an optimistic-concurrency conflict is retryable.
func (s *Server) tryAppend(ctx context.Context, transferID string, expectedSeq int64, event proto.Message) (ok bool, err error) {
	switch err := s.store.Append(ctx, AggregateType, transferID, expectedSeq, event); {
	case err == nil:
		return true, nil
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		return false, nil
	default:
		return false, twirp.InternalErrorWith(err)
	}
}

// sagaStepError maps an error from a claimed saga step (commit,
// cancelStaged, cancelPrepared) to the RPC response: a refused takeover of
// another transition's abandoned claim (errAbandonedClaim, go/docs/adr/0005)
// keeps its FailedPrecondition so the client knows the Transfer is waiting
// on that other transition, not that the server failed; anything else is
// Internal, as before.
func sagaStepError(err error) error {
	if errors.Is(err, errAbandonedClaim) {
		return err
	}
	return twirp.InternalErrorWith(err)
}

func decodedCommitted(events []eventstore.Event) (*pb.TransferCommitted, error) {
	for _, e := range events {
		if e.EventType != eventstore.EventType(&pb.TransferCommitted{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		committed, ok := msg.(*pb.TransferCommitted)
		if !ok {
			return nil, twirp.InternalError(fmt.Sprintf("event typed %s did not decode as TransferCommitted", e.EventType))
		}
		return committed, nil
	}
	return nil, twirp.InternalError("no TransferCommitted event found")
}

// RequestTransfer accepts or rejects req, in both cases recording that
// decision as transferID's first event — a rejection is as much a fact
// about this id's history as an acceptance is, so it belongs in the log
// too, not only in the transient RPC response.
//
// Accepting is all it does. Nothing is prepared, staged or committed here:
// TransferRequestAccepted is published like any other event, and the
// orchestrator's fold of it is what prepares the Transfer and then stages or
// commits it depending on Stage. So an accepted response means the Transfer
// exists and will run, not that any money has moved.
//
// Idempotent: a Transfer that was already decided — accepted or rejected — has
// its recorded outcome returned as-is.
func (s *Server) RequestTransfer(ctx context.Context, req *pb.RequestTransferRequest) (*pb.RequestTransferResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}
	if err := id.Validate("from_wallet_id", req.GetFromWalletId()); err != nil {
		return nil, err
	}
	if err := id.Validate("to_wallet_id", req.GetToWalletId()); err != nil {
		return nil, err
	}
	if err := money.Validate("amount", req.GetAmount()); err != nil {
		return nil, err
	}
	if req.GetMintSource() && req.GetTransactionId() == "" {
		return nil, twirp.InvalidArgumentError("mint_source",
			"requires transaction_id: only Transaction-owned Transfers may mint a source Token")
	}

	for attempt := 0; attempt < maxConcurrencyAttempts; attempt++ {
		events, err := s.store.Load(ctx, AggregateType, req.GetId())
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		if len(events) > 0 {
			return s.decidedRequestTransfer(req.GetId(), events)
		}

		var rejection *pb.TransferRequestRejected
		if req.GetMintSource() {
			rejection, err = validateMintSource(ctx, s.store, s.transactionExists, req.GetTransactionId(), req.GetFromWalletId())
		} else {
			_, rejection, err = selectSourceTokens(ctx, s.store, s.ledger, req.GetFromWalletId(), req.GetAmount(), req.GetTransactionId(), s.isOpen)
		}
		if err != nil {
			return nil, err
		}

		var event proto.Message
		if rejection != nil {
			rejection.Id = req.GetId()
			event = rejection
		} else {
			event = &pb.TransferRequestAccepted{
				Id: req.GetId(), FromWalletId: req.GetFromWalletId(), ToWalletId: req.GetToWalletId(),
				Amount: req.GetAmount(), Stage: req.GetStage(),
				TransactionId: req.GetTransactionId(), MintSource: req.GetMintSource(),
			}
		}

		ok, err := s.tryAppend(ctx, req.GetId(), 0, event)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // a concurrent write landed first — reload and re-decide
		}
		if accepted, isAccept := event.(*pb.TransferRequestAccepted); isAccept {
			return acceptedResponse(accepted), nil
		}
		return rejectedTransferResponse(rejection), nil
	}
	return nil, abortedRetry(req.GetId())
}

// decidedRequestTransfer returns the response for a Transfer that's already
// been decided — accepted or rejected — reading whichever fact actually
// landed rather than assuming which one this caller expected.
func (s *Server) decidedRequestTransfer(transferID string, events []eventstore.Event) (*pb.RequestTransferResponse, error) {
	msg, err := events[0].Decode()
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	switch m := msg.(type) {
	case *pb.TransferRequestAccepted:
		return acceptedResponse(m), nil
	case *pb.TransferRequestRejected:
		return rejectedTransferResponse(m), nil
	default:
		return nil, twirp.InternalError(fmt.Sprintf(
			"transfer %q: stream starts with %s, want TransferRequestAccepted or TransferRequestRejected", transferID, events[0].EventType,
		))
	}
}

func acceptedResponse(e *pb.TransferRequestAccepted) *pb.RequestTransferResponse {
	return &pb.RequestTransferResponse{Id: e.GetId(), Result: &pb.RequestTransferResponse_TransferRequestAccepted{TransferRequestAccepted: e}}
}

func rejectedTransferResponse(r *pb.TransferRequestRejected) *pb.RequestTransferResponse {
	return &pb.RequestTransferResponse{Id: r.GetId(), Result: &pb.RequestTransferResponse_TransferRequestRejected{TransferRequestRejected: r}}
}

// WouldAcceptTransfer exposes the package-level WouldAcceptTransfer as a
// method, over this Server's own store/ledger/isOpen, so transaction.Server
// can call it in-process exactly the way it already calls RequestTransfer —
// without transaction.Server ever needing a ledger.Client of its own.
func (s *Server) WouldAcceptTransfer(
	ctx context.Context, walletID string, amount *sharedpb.Money, callingTransactionID string,
) (*pb.TransferRequestRejected, error) {
	return WouldAcceptTransfer(ctx, s.store, s.ledger, s.isOpen, walletID, amount, callingTransactionID)
}

// CancelAcceptedTransfer abandons a Transfer that hasn't been staged or
// committed yet — legal from Accepted or Prepared. Already-cancelled is an
// idempotent no-op; any later state is refused.
func (s *Server) CancelAcceptedTransfer(ctx context.Context, req *pb.CancelAcceptedTransferRequest) (*pb.CancelAcceptedTransferResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxConcurrencyAttempts; attempt++ {
		events, err := s.store.Load(ctx, AggregateType, req.GetId())
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		if len(events) == 0 {
			return nil, twirp.NewError(twirp.NotFound, fmt.Sprintf("transfer %q not found", req.GetId()))
		}

		switch currentState(events) {
		case stateCancelled:
			return acceptedTransferCancelledResponse(req.GetId(), req.GetReason()), nil
		case stateAccepted, statePrepared:
			if err := s.cancelPrepared(ctx, req.GetId(), req.GetReason()); err != nil {
				return nil, sagaStepError(err)
			}
			events, err = s.store.Load(ctx, AggregateType, req.GetId())
			if err != nil {
				return nil, twirp.InternalErrorWith(err)
			}
			if currentState(events) == stateCancelled {
				return acceptedTransferCancelledResponse(req.GetId(), req.GetReason()), nil
			}
			// cancelPrepared()'s claim (go/docs/adr/0005) was lost to a
			// concurrent caller — possibly one that staged or committed
			// instead. Reload and re-decide rather than assuming
			// cancellation landed.
			continue
		default:
			return nil, twirp.NewError(twirp.FailedPrecondition,
				fmt.Sprintf("transfer %q cannot be cancelled from its current state", req.GetId()))
		}
	}
	return nil, abortedRetry(req.GetId())
}

func acceptedTransferCancelledResponse(transferID, reason string) *pb.CancelAcceptedTransferResponse {
	return &pb.CancelAcceptedTransferResponse{
		Id: transferID,
		Result: &pb.CancelAcceptedTransferResponse_AcceptedTransferCancelled{
			AcceptedTransferCancelled: &pb.AcceptedTransferCancelled{Id: transferID, Reason: reason},
		},
	}
}

// RequestReversal accepts or rejects a reversal of transfer_id, recording
// that decision as this reversal id's first event for the same reason
// RequestTransfer does. If accepted, its manifest is derived from the
// original Transfer (reversalManifest), never independently selected or
// minted, and — like RequestTransfer — it is left for the orchestrator to run
// from the acceptance this publishes.
func (s *Server) RequestReversal(ctx context.Context, req *pb.RequestReversalRequest) (*pb.RequestReversalResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}
	if err := id.Validate("transfer_id", req.GetTransferId()); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxConcurrencyAttempts; attempt++ {
		events, err := s.store.Load(ctx, AggregateType, req.GetId())
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		if len(events) > 0 {
			return s.decidedRequestReversal(req.GetId(), events)
		}

		legs, rejection, err := reversalManifest(ctx, s.store, req.GetTransferId())
		if err != nil {
			return nil, err
		}

		var event proto.Message
		if rejection != nil {
			rejection.Id = req.GetId()
			event = rejection
		} else {
			var total uint64
			currency := ""
			for _, l := range legs {
				total += l.Amount.GetMinorUnits()
				currency = l.Amount.GetCurrency()
			}
			event = &pb.ReversalRequestAccepted{
				Id: req.GetId(), TransferId: req.GetTransferId(),
				Amount: &sharedpb.Money{MinorUnits: total, Currency: currency}, Stage: req.GetStage(),
				TransactionId: req.GetTransactionId(),
			}
		}

		ok, err := s.tryAppend(ctx, req.GetId(), 0, event)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if accepted, isAccept := event.(*pb.ReversalRequestAccepted); isAccept {
			return reversalAcceptedResponse(accepted), nil
		}
		return rejectedReversalResponse(rejection), nil
	}
	return nil, abortedRetry(req.GetId())
}

func (s *Server) decidedRequestReversal(transferID string, events []eventstore.Event) (*pb.RequestReversalResponse, error) {
	msg, err := events[0].Decode()
	if err != nil {
		return nil, twirp.InternalErrorWith(err)
	}
	switch m := msg.(type) {
	case *pb.ReversalRequestAccepted:
		return reversalAcceptedResponse(m), nil
	case *pb.ReversalRequestRejected:
		return rejectedReversalResponse(m), nil
	default:
		return nil, twirp.InternalError(fmt.Sprintf(
			"transfer %q: stream starts with %s, want ReversalRequestAccepted or ReversalRequestRejected", transferID, events[0].EventType,
		))
	}
}

func reversalAcceptedResponse(e *pb.ReversalRequestAccepted) *pb.RequestReversalResponse {
	return &pb.RequestReversalResponse{Id: e.GetId(), Result: &pb.RequestReversalResponse_ReversalRequestAccepted{ReversalRequestAccepted: e}}
}

func rejectedReversalResponse(r *pb.ReversalRequestRejected) *pb.RequestReversalResponse {
	return &pb.RequestReversalResponse{Id: r.GetId(), Result: &pb.RequestReversalResponse_ReversalRequestRejected{ReversalRequestRejected: r}}
}

// ConfirmStagedTransfer moves a Transfer from Staged to Pending: ruby's own
// provisioning call to the external provider succeeded. Pure event-log
// write — see confirmStaged. Idempotent from Pending; a rejection from any
// other state is itself recorded onto the Transfer's stream (not just
// returned), so "ruby tried to confirm a transfer that wasn't staged" is
// part of its durable history.
func (s *Server) ConfirmStagedTransfer(ctx context.Context, req *pb.ConfirmStagedTransferRequest) (*pb.ConfirmStagedTransferResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxConcurrencyAttempts; attempt++ {
		events, err := s.store.Load(ctx, AggregateType, req.GetId())
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		if len(events) == 0 {
			// No aggregate exists for this id at all, so — unlike the
			// rejections below — there's no stream to record this refusal
			// onto; it stays a transient response, the same as before.
			return &pb.ConfirmStagedTransferResponse{
				Id: req.GetId(),
				Result: &pb.ConfirmStagedTransferResponse_ConfirmStagedTransferRejected{
					ConfirmStagedTransferRejected: &pb.ConfirmStagedTransferRejected{Id: req.GetId(), Reason: "transfer not found"},
				},
			}, nil
		}

		switch state := currentState(events); state {
		case stateStaged:
			if err := s.confirmStaged(ctx, req.GetId()); err != nil {
				return nil, twirp.InternalErrorWith(err)
			}
			return transferPendingResponse(req.GetId()), nil
		case statePending:
			return transferPendingResponse(req.GetId()), nil
		default:
			rejection := &pb.ConfirmStagedTransferRejected{Id: req.GetId(), Reason: fmt.Sprintf("transfer is %s, not staged", state)}
			ok, err := s.tryAppend(ctx, req.GetId(), int64(len(events)), rejection)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			return &pb.ConfirmStagedTransferResponse{
				Id:     req.GetId(),
				Result: &pb.ConfirmStagedTransferResponse_ConfirmStagedTransferRejected{ConfirmStagedTransferRejected: rejection},
			}, nil
		}
	}
	return nil, abortedRetry(req.GetId())
}

func transferPendingResponse(transferID string) *pb.ConfirmStagedTransferResponse {
	return &pb.ConfirmStagedTransferResponse{
		Id:     transferID,
		Result: &pb.ConfirmStagedTransferResponse_TransferPending{TransferPending: &pb.TransferPending{Id: transferID}},
	}
}

// CancelStagedTransfer moves a Transfer to Cancelled from either Staged or
// Pending — an external factor stopped it (ruby's provisioning failed, or
// the provider reported the in-flight transfer didn't go through). See
// cancelStaged and decision #13. Idempotent from Cancelled; a rejection from
// any other state is recorded onto the Transfer's stream, the same reason
// as ConfirmStagedTransfer's rejection is.
func (s *Server) CancelStagedTransfer(ctx context.Context, req *pb.CancelStagedTransferRequest) (*pb.CancelStagedTransferResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxConcurrencyAttempts; attempt++ {
		events, err := s.store.Load(ctx, AggregateType, req.GetId())
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		if len(events) == 0 {
			// No aggregate exists for this id at all, so — unlike the
			// rejections below — there's no stream to record this refusal
			// onto; it stays a transient response, the same as before.
			return &pb.CancelStagedTransferResponse{
				Id: req.GetId(),
				Result: &pb.CancelStagedTransferResponse_CancelStagedTransferRejected{
					CancelStagedTransferRejected: &pb.CancelStagedTransferRejected{Id: req.GetId(), Reason: "transfer not found"},
				},
			}, nil
		}

		switch state := currentState(events); state {
		case stateStaged, statePending:
			if err := s.cancelStaged(ctx, req.GetId(), req.GetReason()); err != nil {
				return nil, sagaStepError(err)
			}
			events, err = s.store.Load(ctx, AggregateType, req.GetId())
			if err != nil {
				return nil, twirp.InternalErrorWith(err)
			}
			if currentState(events) == stateCancelled {
				return transferCancelledResponse(req.GetId(), req.GetReason()), nil
			}
			// cancelStaged()'s claim (go/docs/adr/0005) was lost to a
			// concurrent caller — possibly one that committed instead.
			// Reload and re-decide rather than assuming cancellation landed.
			continue
		case stateCancelled:
			return transferCancelledResponse(req.GetId(), req.GetReason()), nil
		default:
			rejection := &pb.CancelStagedTransferRejected{Id: req.GetId(), Reason: fmt.Sprintf("transfer is %s, cannot be cancelled from there", state)}
			ok, err := s.tryAppend(ctx, req.GetId(), int64(len(events)), rejection)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			return &pb.CancelStagedTransferResponse{
				Id:     req.GetId(),
				Result: &pb.CancelStagedTransferResponse_CancelStagedTransferRejected{CancelStagedTransferRejected: rejection},
			}, nil
		}
	}
	return nil, abortedRetry(req.GetId())
}

func transferCancelledResponse(transferID, reason string) *pb.CancelStagedTransferResponse {
	return &pb.CancelStagedTransferResponse{
		Id: transferID,
		Result: &pb.CancelStagedTransferResponse_TransferCancelled{
			TransferCancelled: &pb.TransferCancelled{Id: transferID, Reason: reason},
		},
	}
}

// PostPendingTransfer moves a Transfer from Pending to Committed ("posted"):
// the provider confirmed the funds posted. See commit's posting variant.
// Idempotent from Committed; a rejection from any other state is recorded
// onto the Transfer's stream, the same reason as ConfirmStagedTransfer's
// rejection is. Unlike the other staged-flow RPCs, a TigerBeetle-level
// rejection discovered here is reported as this call's own error rather
// than swallowed — this call's whole job is to synchronously finalize, so
// it can (and should) report that it didn't, even though the same
// rejection reaching commit() via RequestTransfer's immediate path cannot
// (decision #7).
func (s *Server) PostPendingTransfer(ctx context.Context, req *pb.PostPendingTransferRequest) (*pb.PostPendingTransferResponse, error) {
	if err := id.Validate("id", req.GetId()); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxConcurrencyAttempts; attempt++ {
		events, err := s.store.Load(ctx, AggregateType, req.GetId())
		if err != nil {
			return nil, twirp.InternalErrorWith(err)
		}
		if len(events) == 0 {
			// No aggregate exists for this id at all, so — unlike the
			// rejections below — there's no stream to record this refusal
			// onto; it stays a transient response, the same as before.
			return &pb.PostPendingTransferResponse{
				Id: req.GetId(),
				Result: &pb.PostPendingTransferResponse_PostPendingTransferRejected{
					PostPendingTransferRejected: &pb.PostPendingTransferRejected{Id: req.GetId(), Reason: "transfer not found"},
				},
			}, nil
		}

		switch state := currentState(events); state {
		case statePending:
			if err := s.commit(ctx, req.GetId()); err != nil {
				return nil, sagaStepError(err)
			}
			events, err = s.store.Load(ctx, AggregateType, req.GetId())
			if err != nil {
				return nil, twirp.InternalErrorWith(err)
			}
			switch currentState(events) {
			case stateCommitted:
				return postPendingCommittedResponse(req.GetId(), events)
			case stateFailed:
				return nil, twirp.NewError(twirp.Internal, fmt.Sprintf(
					"transfer %q: tigerbeetle rejected posting; see TransferFailed in the event log", req.GetId(),
				))
			default:
				// Still pending: commit()'s claim (go/docs/adr/0005) was lost
				// to a concurrent caller who hasn't finished yet. Reload and
				// retry rather than assuming an outcome that hasn't landed.
				continue
			}
		case stateCommitted:
			return postPendingCommittedResponse(req.GetId(), events)
		default:
			rejection := &pb.PostPendingTransferRejected{Id: req.GetId(), Reason: fmt.Sprintf("transfer is %s, not pending", state)}
			ok, err := s.tryAppend(ctx, req.GetId(), int64(len(events)), rejection)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			return &pb.PostPendingTransferResponse{
				Id:     req.GetId(),
				Result: &pb.PostPendingTransferResponse_PostPendingTransferRejected{PostPendingTransferRejected: rejection},
			}, nil
		}
	}
	return nil, abortedRetry(req.GetId())
}

func postPendingCommittedResponse(transferID string, events []eventstore.Event) (*pb.PostPendingTransferResponse, error) {
	committed, err := decodedCommitted(events)
	if err != nil {
		return nil, err
	}
	return &pb.PostPendingTransferResponse{
		Id:     transferID,
		Result: &pb.PostPendingTransferResponse_TransferCommitted{TransferCommitted: committed},
	}, nil
}
