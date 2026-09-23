package main

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// The tests in this package run the tool in-process, with no Kafka and no
// cmd/orchestrator. Since the async cutover that leaves nothing to drive a
// saga: every RPC records its decision and returns, and the events that would
// have woken the orchestrator go nowhere.
//
// drivenTransactions and drivenTransfers stand in for it, by resuming through
// the real saga.Orchestrator the moment a call has written something worth
// waking on. That is the same fold production performs, minus the transport —
// what a delivered trigger would have caused, caused directly instead. It is a
// stand-in for the delivery, not for the driving.
//
// Against a real server this is not needed and not wanted: the tool talks to
// cmd/server over HTTP and cmd/orchestrator does this job for real, which is
// why -settle-wait-attempts exists.
type drivenTransactions struct {
	transactionpb.TransactionService
	orchestrator *saga.Orchestrator
	store        eventstore.Store
}

func (d drivenTransactions) StartInitializingTransaction(
	ctx context.Context, req *transactionpb.StartInitializingTransactionRequest,
) (*transactionpb.StartInitializingTransactionResponse, error) {
	resp, err := d.TransactionService.StartInitializingTransaction(ctx, req)
	if err != nil || resp.GetTransactionInitialized() == nil {
		return resp, err
	}
	return resp, d.drive(ctx, req.GetId())
}

func (d drivenTransactions) StartProcessingTransfer(
	ctx context.Context, req *transactionpb.StartProcessingTransferRequest,
) (*transactionpb.StartProcessingTransferResponse, error) {
	resp, err := d.TransactionService.StartProcessingTransfer(ctx, req)
	if err != nil {
		return resp, err
	}
	return resp, d.drive(ctx, req.GetId())
}

func (d drivenTransactions) StartTransactionRollback(
	ctx context.Context, req *transactionpb.StartTransactionRollbackRequest,
) (*transactionpb.StartTransactionRollbackResponse, error) {
	resp, err := d.TransactionService.StartTransactionRollback(ctx, req)
	if err != nil {
		return resp, err
	}
	if err := d.drive(ctx, req.GetId()); err != nil {
		return resp, err
	}
	// The rollback's own progress changed the state this call reports, so read
	// it again rather than answering with what was true before the drive.
	state, err := d.TransactionService.GetTransactionState(ctx, &transactionpb.GetTransactionStateRequest{Id: req.GetId()})
	if err != nil {
		return resp, err
	}
	return &transactionpb.StartTransactionRollbackResponse{Id: state.GetId(), State: state.GetState()}, nil
}

func (d drivenTransactions) GetTransactionState(
	ctx context.Context, req *transactionpb.GetTransactionStateRequest,
) (*transactionpb.GetTransactionStateResponse, error) {
	if err := d.drive(ctx, req.GetId()); err != nil {
		return nil, err
	}
	return d.TransactionService.GetTransactionState(ctx, req)
}

// drive delivers what the transaction topic would have delivered, repeatedly,
// until nothing moves. Reversals and staged children mean one wake-up is rarely
// enough.
// drive delivers what both topics would have delivered for this Transaction:
// its own trigger, then one per child Transfer that has written anything, round
// after round until nothing moves. Both halves are needed — a transaction
// trigger dispatches a child but never runs it, because running it is what the
// child's own acceptance triggers.
func (d drivenTransactions) drive(ctx context.Context, transactionID string) error {
	const maxRounds = 100
	for round := 0; round < maxRounds; round++ {
		before, err := d.totalEvents(ctx, transactionID)
		if err != nil {
			return err
		}

		if err := handleWithRetries(ctx, d.orchestrator, saga.Trigger{
			AggregateType: transaction.AggregateType, AggregateID: transactionID,
		}); err != nil {
			return err
		}
		children, err := d.childTransfers(ctx, transactionID)
		if err != nil {
			return err
		}
		for _, childID := range children {
			if err := handleWithRetries(ctx, d.orchestrator, saga.Trigger{
				AggregateType: transfer.AggregateType, AggregateID: childID,
			}); err != nil {
				return err
			}
		}

		after, err := d.totalEvents(ctx, transactionID)
		if err != nil {
			return err
		}
		if after == before {
			return nil
		}
	}
	return fmt.Errorf("simulate: transaction %s still moving after %d rounds", transactionID, maxRounds)
}

// childTransfers lists every Transfer this Transaction has written about that
// has a stream of its own — Reversals included, gated children excluded (they
// have no stream, so no trigger would exist for them).
func (d drivenTransactions) childTransfers(ctx context.Context, transactionID string) ([]string, error) {
	events, err := d.store.Load(ctx, transaction.AggregateType, transactionID)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var ids []string
	for _, e := range events {
		msg, err := e.Decode()
		if err != nil {
			return nil, err
		}
		for _, id := range namedTransferIDs(msg) {
			if id == "" || seen[id] {
				continue
			}
			child, err := d.store.Load(ctx, transfer.AggregateType, id)
			if err != nil {
				return nil, err
			}
			if len(child) > 0 {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (d drivenTransactions) totalEvents(ctx context.Context, transactionID string) (int, error) {
	events, err := d.store.Load(ctx, transaction.AggregateType, transactionID)
	if err != nil {
		return 0, err
	}
	total := len(events)
	children, err := d.childTransfers(ctx, transactionID)
	if err != nil {
		return 0, err
	}
	for _, childID := range children {
		child, err := d.store.Load(ctx, transfer.AggregateType, childID)
		if err != nil {
			return 0, err
		}
		total += len(child)
	}
	return total, nil
}

func namedTransferIDs(msg proto.Message) []string {
	switch m := msg.(type) {
	case *transactionpb.TransferRequestedWithinTransaction:
		return []string{m.GetTransferId()}
	case *transactionpb.TransferGatedWithinTransaction:
		return []string{m.GetTransferId()}
	case *transactionpb.TransferCompletedWithinTransaction:
		return []string{m.GetTransferId()}
	case *transactionpb.TransferFailedWithinTransaction:
		return []string{m.GetTransferId()}
	case *transactionpb.TransferReversalRequestedWithinTransaction:
		return []string{m.GetTransferId(), m.GetReversalId()}
	case *transactionpb.TransferRolledBackWithinTransaction:
		return []string{m.GetTransferId(), m.GetDetailId()}
	case *transactionpb.TransferRollbackFailedWithinTransaction:
		return []string{m.GetTransferId()}
	default:
		return nil
	}
}

type drivenTransfers struct {
	transferpb.TransferService
	orchestrator *saga.Orchestrator
	store        eventstore.Store
}

func (d drivenTransfers) RequestTransfer(
	ctx context.Context, req *transferpb.RequestTransferRequest,
) (*transferpb.RequestTransferResponse, error) {
	resp, err := d.TransferService.RequestTransfer(ctx, req)
	if err != nil || resp.GetTransferRequestAccepted() == nil {
		return resp, err
	}
	return resp, d.drive(ctx, req.GetId())
}

func (d drivenTransfers) RequestReversal(
	ctx context.Context, req *transferpb.RequestReversalRequest,
) (*transferpb.RequestReversalResponse, error) {
	resp, err := d.TransferService.RequestReversal(ctx, req)
	if err != nil || resp.GetReversalRequestAccepted() == nil {
		return resp, err
	}
	return resp, d.drive(ctx, req.GetId())
}

func (d drivenTransfers) ConfirmStagedTransfer(
	ctx context.Context, req *transferpb.ConfirmStagedTransferRequest,
) (*transferpb.ConfirmStagedTransferResponse, error) {
	resp, err := d.TransferService.ConfirmStagedTransfer(ctx, req)
	if err != nil {
		return resp, err
	}
	return resp, d.drive(ctx, req.GetId())
}

func (d drivenTransfers) PostPendingTransfer(
	ctx context.Context, req *transferpb.PostPendingTransferRequest,
) (*transferpb.PostPendingTransferResponse, error) {
	resp, err := d.TransferService.PostPendingTransfer(ctx, req)
	if err != nil {
		return resp, err
	}
	return resp, d.drive(ctx, req.GetId())
}

func (d drivenTransfers) CancelStagedTransfer(
	ctx context.Context, req *transferpb.CancelStagedTransferRequest,
) (*transferpb.CancelStagedTransferResponse, error) {
	resp, err := d.TransferService.CancelStagedTransfer(ctx, req)
	if err != nil {
		return resp, err
	}
	return resp, d.drive(ctx, req.GetId())
}

func (d drivenTransfers) drive(ctx context.Context, transferID string) error {
	return driveUntilQuiet(ctx, d.orchestrator, d.store, transfer.AggregateType, transferID)
}

// handleWithRetries retries a failed trigger a few times before giving up, the
// way cmd/orchestrator does before it halts (go/docs/adr/0003). It matters here
// for the same reason it matters there: a Transfer whose prepare step loses a
// concurrency race on a hot Wallet fails its handler and succeeds on the next
// attempt, and treating that as fatal would make this test fail on scheduling
// luck rather than on anything about the code.
func handleWithRetries(ctx context.Context, o *saga.Orchestrator, t saga.Trigger) error {
	const attempts = 4
	var err error
	for i := 0; i < attempts; i++ {
		if err = o.Handle(ctx, t); err == nil {
			return nil
		}
	}
	return err
}

// driveUntilQuiet redelivers the same trigger until the named aggregate's own
// stream stops growing. Redelivery is what at-least-once already allows, and
// one wake-up is rarely enough: a slice boundary, a staged child or a reversal
// each leave more to do. Bounded, so a saga that genuinely will not converge
// fails rather than spins.
func driveUntilQuiet(ctx context.Context, o *saga.Orchestrator, store eventstore.Store, aggregateType, aggregateID string) error {
	trigger := saga.Trigger{AggregateType: aggregateType, AggregateID: aggregateID}

	const maxWakeUps = 100
	for i := 0; i < maxWakeUps; i++ {
		before, err := store.Load(ctx, aggregateType, aggregateID)
		if err != nil {
			return err
		}
		if err := handleWithRetries(ctx, o, trigger); err != nil {
			return err
		}
		after, err := store.Load(ctx, aggregateType, aggregateID)
		if err != nil {
			return err
		}
		if len(after) == len(before) {
			return nil
		}
	}
	return fmt.Errorf("simulate: %s %s still moving after %d wake-ups", aggregateType, aggregateID, maxWakeUps)
}
