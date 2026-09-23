package main

import (
	"context"
	"testing"
	"uuid"

	holderpb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/holder"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// chainedTransaction accepts a two-hop Transaction — second depends on first,
// both minting from a reserve — and nothing more. No trigger is ever
// published here, which is the situation cmd/resume exists for: accepted
// work whose wake-ups never arrived.
func chainedTransaction(t *testing.T) (d driver, store eventstore.Store, transactionID, first, second string) {
	t.Helper()
	ctx := context.Background()
	store = eventstore.NewMemoryStore()
	servers := saga.Wire(store, ledger.NewFakeClient())

	holders := holder.NewServer(store)
	reserve, target := uuid.NewV7().String(), uuid.NewV7().String()
	for wallet, allows := range map[string]sharedpb.Allows{reserve: sharedpb.Allows_ALLOWS_ONRAMP, target: sharedpb.Allows_ALLOWS_NONE} {
		resp, err := holders.Provision(ctx, &holderpb.ProvisionRequest{
			Id: uuid.NewV7().String(), Wallets: []*holderpb.WalletSpec{{WalletId: wallet, Name: "w", Allows: allows}},
		})
		if err != nil || resp.GetHolderProvisionRejected() != nil {
			t.Fatalf("Provision() = (%v, %v)", resp.GetResult(), err)
		}
	}

	transactionID, first, second = uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String()
	usd := &sharedpb.Money{MinorUnits: 100, Currency: "USD"}
	resp, err := servers.Transaction.StartInitializingTransaction(ctx, &transactionpb.StartInitializingTransactionRequest{
		Id: transactionID,
		Transfers: map[string]*transactionpb.Transfer{
			first:  {Id: first, Amount: usd, FromWalletId: reserve, ToWalletId: target, AutoProcess: true, MintSource: true},
			second: {Id: second, Amount: usd, FromWalletId: reserve, ToWalletId: target, AutoProcess: true, MintSource: true},
		},
		TransferDependency: map[string]*transactionpb.TransferIdList{second: {TransferId: []string{first}}},
	})
	if err != nil || resp.GetTransactionInitialized() == nil {
		t.Fatalf("StartInitializingTransaction() = (%v, %v)", resp.GetResult(), err)
	}
	return driver{orchestrator: servers.Orchestrator(), store: store}, store, transactionID, first, second
}

func assertCompleted(t *testing.T, store eventstore.Store, transactionID string) {
	t.Helper()
	open, err := transaction.IsOpen(context.Background(), store, transactionID)
	if err != nil {
		t.Fatalf("IsOpen() error = %v", err)
	}
	events, err := store.Load(context.Background(), transaction.AggregateType, transactionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	last := events[len(events)-1].EventType
	if open || last != eventstore.EventType(&transactionpb.TransactionCompleted{}) {
		t.Errorf("transaction still open=%v, last event %s; want it completed in one drive", open, last)
	}
}

// Nothing will ever publish a trigger for the children this Transaction
// dispatches while it is being driven, so driving it has to wake them too —
// or every hop of the DAG costs another run of the tool.
func TestDrive_CarriesATransactionThroughEveryHopInOneRun(t *testing.T) {
	t.Parallel()
	d, store, transactionID, _, second := chainedTransaction(t)

	rounds, err := d.drive(context.Background(), target{aggregateType: transaction.AggregateType, aggregateID: transactionID})
	if err != nil {
		t.Fatalf("drive() error = %v", err)
	}
	if rounds < 2 {
		t.Errorf("rounds = %d, want more than one: it advanced", rounds)
	}
	assertCompleted(t, store, transactionID)
	if outcome, err := transfer.Outcome(context.Background(), store, second); err != nil || outcome != transfer.OutcomeCommitted {
		t.Errorf("second Outcome() = (%v, %v), want committed", outcome, err)
	}
}

// Naming a child Transfer is naming its Transaction's progress too: the
// Transaction moving is what the operator asked for, so it counts as advance
// and is carried through, not logged as inert.
func TestDrive_CarriesTheOwningTransactionWhenDrivingAChild(t *testing.T) {
	t.Parallel()
	d, store, transactionID, first, _ := chainedTransaction(t)
	ctx := context.Background()

	// Get it to the point the tool finds it at: started, first leg accepted
	// and never run.
	if err := d.orchestrator.Handle(ctx, saga.Trigger{AggregateType: transaction.AggregateType, AggregateID: transactionID}); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if outcome, _ := transfer.Outcome(ctx, store, first); outcome != transfer.OutcomeInFlight {
		t.Fatalf("first Outcome() = %v, want in_flight before driving", outcome)
	}

	rounds, err := d.drive(ctx, target{aggregateType: transfer.AggregateType, aggregateID: first})
	if err != nil {
		t.Fatalf("drive() error = %v", err)
	}
	if rounds < 2 {
		t.Errorf("rounds = %d, want more than one: driving the child advanced its Transaction", rounds)
	}
	assertCompleted(t, store, transactionID)
}
