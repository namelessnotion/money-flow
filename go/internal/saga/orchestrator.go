package saga

import (
	"context"
	"fmt"

	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// Resumer advances one aggregate's saga from whatever its own stream currently
// records. Both transfer.Server and transaction.Server implement it, and the
// orchestrator needs nothing else from either: a trigger names an aggregate,
// and resuming it is the whole response.
type Resumer interface {
	Resume(ctx context.Context, aggregateID string) error
}

// OwnerResolver reports which Transaction drove transferID, or "" when none
// did. transfer.OwningTransaction is the implementation; it is taken as a
// function so this package depends on the answer rather than on how the
// Transfer aggregate stores it.
type OwnerResolver func(ctx context.Context, transferID string) (string, error)

// Orchestrator turns triggers into saga progress.
type Orchestrator struct {
	transfers         Resumer
	transactions      Resumer
	owningTransaction OwnerResolver
}

func New(transfers, transactions Resumer, owningTransaction OwnerResolver) *Orchestrator {
	return &Orchestrator{transfers: transfers, transactions: transactions, owningTransaction: owningTransaction}
}

// Handle drives whatever t names.
//
// An aggregate type this orchestrator does not know is an error rather than
// something to skip. It subscribes to exactly the two topics the publication
// contract routes by aggregate_type, so a third kind of message arriving means
// that contract has changed underneath it — a fact worth stopping on, not one
// worth stepping over.
func (o *Orchestrator) Handle(ctx context.Context, t Trigger) error {
	switch t.AggregateType {
	case transfer.AggregateType:
		return o.handleTransfer(ctx, t.AggregateID)
	case transaction.AggregateType:
		return o.transactions.Resume(ctx, t.AggregateID)
	default:
		return fmt.Errorf("saga: no driver for aggregate type %q (%s)", t.AggregateType, t)
	}
}

// handleTransfer resumes the Transfer itself and then the Transaction that
// owns it, if any.
//
// The second half is what makes per-aggregate-type topics workable. A trigger
// on the transfer topic names a Transfer, but "have all my children finished?"
// and "has this Reversal resolved?" are decisions that live on the
// Transaction. Without following the link, a Transaction whose last child just
// committed would never hear about it and would sit in started or
// rollback_started forever — the liveness stall go/docs/adr/0001 measured.
//
// The Transfer is resumed first so the Transaction re-folds against a child
// that is already as far along as this trigger can take it, saving a second
// wake-up in the common case. A Transfer that belongs to no Transaction — a
// standalone request, or one whose stream opens with a rejection — resolves to
// "" and is simply driven on its own.
func (o *Orchestrator) handleTransfer(ctx context.Context, transferID string) error {
	if err := o.transfers.Resume(ctx, transferID); err != nil {
		return err
	}

	transactionID, err := o.owningTransaction(ctx, transferID)
	if err != nil {
		return err
	}
	if transactionID == "" {
		return nil
	}
	return o.transactions.Resume(ctx, transactionID)
}
