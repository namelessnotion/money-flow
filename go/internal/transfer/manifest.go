package transfer

import (
	"context"
	"fmt"

	"github.com/twitchtv/twirp"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/detid"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/token"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

// Leg is one DEBIT/CREDIT pairing within a Transfer: an amount moving from
// SourceTokenID to DestTokenID. selectSourceTokens fills in only
// SourceTokenID and Amount — DestTokenID is joined in once prepare() has
// decided (or, for a forward Transfer, minted) the destination(s).
type Leg struct {
	SourceTokenID string
	DestTokenID   string
	Amount        *sharedpb.Money
}

// selectSourceTokens picks existing Token(s) belonging to walletID, oldest-
// minted-first (FIFO), accumulating until amount is covered. This is what
// produces the many-to-one case when a single Token's balance can't cover
// the full amount, and the one-to-one case when it can.
//
// ASSUMPTION — source Token selection strategy: oldest-minted-first. This is
// a documented, swappable choice, isolated to this one function; an
// alternative (e.g. largest-balance-first, to minimize the number of legs)
// is easy to substitute here without touching anything else.
//
// callingTransactionID and isOpen thread through to wallet.TokensOfVisibleTo
// so a Token tagged with a different, still-open Transaction is invisible to
// this selection — the mechanism that keeps a Transaction's own compensation
// from racing a later, unrelated caller that spends the same Token down
// first. callingTransactionID is empty and isOpen is nil for a standalone
// Transfer, which degrades to the old unfiltered behavior.
//
// Returns a domain rejection, not an error, when the Wallet's total existing
// Token capacity can't cover amount.
func selectSourceTokens(
	ctx context.Context, store eventstore.Store, lc ledger.Client, walletID string, amount *sharedpb.Money,
	callingTransactionID string, isOpen wallet.TransactionOpenChecker,
) ([]Leg, *pb.TransferRequestRejected, error) {
	tokenIDs, err := wallet.TokensOfVisibleTo(ctx, store, walletID, callingTransactionID, isOpen)
	if err != nil {
		return nil, nil, err
	}

	balances, err := lc.Balances(ctx, tokenIDs)
	if err != nil {
		return nil, nil, twirp.InternalErrorWith(fmt.Errorf("ledger: Balances: %w", err))
	}

	var legs []Leg
	remaining := amount.GetMinorUnits()
	for _, tokenID := range tokenIDs {
		if remaining == 0 {
			break
		}
		b, found := balances[tokenID]
		if !found {
			continue
		}
		balance, err := b.PostedNet()
		if err != nil {
			return nil, nil, twirp.InternalErrorWith(fmt.Errorf("ledger: token %q: %w", tokenID, err))
		}
		if balance <= 0 {
			continue
		}
		take := uint64(balance)
		if take > remaining {
			take = remaining
		}
		legs = append(legs, Leg{
			SourceTokenID: tokenID,
			Amount:        &sharedpb.Money{MinorUnits: take, Currency: amount.GetCurrency()},
		})
		remaining -= take
	}

	if remaining > 0 {
		return nil, &pb.TransferRequestRejected{
			Reason: fmt.Sprintf(
				"wallet %q has insufficient Token capacity: %d %s short",
				walletID, remaining, amount.GetCurrency(),
			),
		}, nil
	}
	return legs, nil, nil
}

// WouldAcceptTransfer reports whether an ordinary (non-mint_source)
// RequestTransfer for walletID/amount, tagged with callingTransactionID,
// would be accepted right now — without writing anything. It is exactly the
// read-only decision RequestTransfer itself makes before ever appending
// TransferRequestAccepted (see selectSourceTokens above), discarding only
// the picked []Leg breakdown that RequestTransfer needs and this caller
// doesn't.
//
// This is NOT a reservation: nothing here locks or consumes the balance it
// reads, so a concurrent, unrelated debit against the same wallet between
// this call and the real dispatch that follows can still change the
// answer. Callers must treat a nil rejection here as "usually right," never
// as a substitute for whatever check the real dispatch performs on its
// own — see go/docs/adr/0004.
func WouldAcceptTransfer(
	ctx context.Context, store eventstore.Store, lc ledger.Client, isOpen wallet.TransactionOpenChecker,
	walletID string, amount *sharedpb.Money, callingTransactionID string,
) (*pb.TransferRequestRejected, error) {
	_, rejection, err := selectSourceTokens(ctx, store, lc, walletID, amount, callingTransactionID, isOpen)
	return rejection, err
}

// planDestinations decides how many new destination Tokens to mint for a
// forward Transfer and how the amount splits across them.
//
// Each id is derived from transferID, not generated: minting creates a
// Token's TigerBeetle account before the append that can lose a race for
// the Wallet, so every planning of one Transfer — a re-plan, a redelivered
// trigger, cmd/resume — has to name the same Tokens, or each lost plan
// leaves an account behind.
//
// PROPOSED ASSUMPTION, per decision #2: v1 always mints exactly one
// destination Token for the full amount. One-to-many is satisfied entirely
// by reversing a many-to-one Transfer instead — see reversalManifest — so
// no "max capacity per Token" splitting rule is needed here.
func planDestinations(transferID string, amount *sharedpb.Money) []token.MintSpec {
	return []token.MintSpec{{TokenID: detid.New(transferID + "/destination-token/0"), Capacity: amount}}
}

// mintSourceLeg builds the MintSpec for a mint_source=true request's sole
// source Token: a brand-new Token minted for the full amount, used directly
// as the DEBIT leg once its stream is created — the mint_source counterpart
// to selectSourceTokens's existing-balance FIFO path. Pure — no I/O, same
// reason planDestinations is pure — the actual TigerBeetle account and
// event writes happen when the caller passes this spec into
// token.MintWrites; its id is derived from transferID for the same reason
// planDestinations derives its own. No balance check here: the debit that
// follows is expected to drive this fresh, zero-balance Token negative,
// which is exactly what the source Wallet's Allows-derived account flags
// (validated by validateMintSource) exist to legally permit.
func mintSourceLeg(transferID string, amount *sharedpb.Money) token.MintSpec {
	return token.MintSpec{TokenID: detid.New(transferID + "/source-token"), Capacity: amount}
}

// TransactionExistsChecker reports whether transactionID names a real,
// already-initialized Transaction. Used only to authorize mint_source=true
// requests — a transaction_id claim that doesn't resolve to an actual
// Transaction stream must not be allowed to mint fresh money into the
// ledger. Distinct from wallet.TransactionOpenChecker (which asks whether a
// TAGGED Token's owning Transaction has closed, defaulting to "still open"
// on a not-found stream as a conservative fail-safe): here, "not found"
// must mean reject, the opposite bias, so the two questions are kept as
// separate checkers rather than one overloaded function.
type TransactionExistsChecker func(ctx context.Context, transactionID string) (bool, error)

// validateMintSource authorizes a mint_source=true request: transactionID
// must name a real, existing Transaction (mint_source is Transaction-only —
// a standalone Transfer can never reach this function at all, since
// RequestTransfer refuses mint_source=true without a transaction_id before
// any store read), and walletID must exist with an Allows policy that
// includes ONRAMP (mint_source is how money enters the ledger, so it is
// only ever legal against a Wallet the business has explicitly marked as a
// funding instrument's boundary). Returns a domain rejection, not an error,
// for either failure — the same shape selectSourceTokens's
// insufficient-capacity rejection already takes. Called both at accept time
// and again in prepare(), mirroring selectSourceTokens's own existing
// re-check there.
func validateMintSource(
	ctx context.Context, store eventstore.Store, transactionExists TransactionExistsChecker,
	transactionID, walletID string,
) (*pb.TransferRequestRejected, error) {
	exists, err := transactionExists(ctx, transactionID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &pb.TransferRequestRejected{Reason: fmt.Sprintf("transaction %q not found", transactionID)}, nil
	}

	opened, err := wallet.Opened(ctx, store, walletID)
	if err != nil {
		return nil, err
	}
	if opened == nil {
		return &pb.TransferRequestRejected{Reason: fmt.Sprintf("wallet %q not found", walletID)}, nil
	}
	switch opened.GetAllows() {
	case sharedpb.Allows_ALLOWS_ONRAMP, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP:
		return nil, nil
	default:
		return &pb.TransferRequestRejected{Reason: fmt.Sprintf(
			"wallet %q does not allow onramp; mint_source requires ALLOWS_ONRAMP or ALLOWS_ONRAMP_AND_OFFRAMP", walletID,
		)}, nil
	}
}

// reversalManifest derives a reversal's legs from the original Transfer's
// own recorded TransferPrepared.legs: original DEBIT (source→dest, amt)
// becomes reversal DEBIT (dest→source, amt), one leg per original leg. No
// Token minting, no independent selection — the destinations are always the
// original Transfer's own source Token(s), which already exist by
// construction. This is why decision #1's repeated-destination shape and
// decision #4's reversal-as-new-Transfer compose for free: an original
// many-to-one forward Transfer (N sources → 1 destination) reverses into a
// one-to-many reversal (1 source → N destinations).
//
// Returns a domain rejection, not an error, when the original Transfer
// doesn't exist or was never committed.
func reversalManifest(ctx context.Context, store eventstore.Store, originalTransferID string) ([]Leg, *pb.ReversalRequestRejected, error) {
	events, err := store.Load(ctx, AggregateType, originalTransferID)
	if err != nil {
		return nil, nil, twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return nil, &pb.ReversalRequestRejected{
			TransferId: originalTransferID, Reason: "original transfer not found",
		}, nil
	}
	if state := currentState(events); state != stateCommitted {
		return nil, &pb.ReversalRequestRejected{
			TransferId: originalTransferID,
			Reason:     fmt.Sprintf("original transfer is %s, not committed", state),
		}, nil
	}

	var legs []Leg
	for _, e := range events {
		if e.EventType != eventstore.EventType(&pb.TransferPrepared{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			return nil, nil, twirp.InternalErrorWith(err)
		}
		prepared, ok := msg.(*pb.TransferPrepared)
		if !ok {
			return nil, nil, twirp.InternalError(fmt.Sprintf(
				"transfer %q: event typed %s did not decode as TransferPrepared", originalTransferID, e.EventType,
			))
		}
		for _, l := range prepared.GetLegs() {
			legs = append(legs, Leg{
				SourceTokenID: l.GetDestTokenId(),
				DestTokenID:   l.GetSourceTokenId(),
				Amount:        l.GetAmount(),
			})
		}
	}
	if len(legs) == 0 {
		return nil, nil, twirp.InternalError(fmt.Sprintf(
			"transfer %q: committed but has no recorded legs", originalTransferID,
		))
	}
	return legs, nil, nil
}
