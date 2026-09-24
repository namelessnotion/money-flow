package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
)

func seedEntities() (entity, []entity) {
	return entity{name: "reserve", walletID: "reserve"}, []entity{
		{name: "entity-0", walletID: "w0"},
		{name: "entity-1", walletID: "w1"},
	}
}

// Accepting a seed is not funding the entity: the load that follows spends the
// seed, so seedAll returns only once the orchestrator has committed every one.
func TestSeedAll_WaitsForEverySeedToComplete(t *testing.T) {
	t.Parallel()
	transactions := newScriptedTransactions(txInitialized, txStarted, txCompleted)
	reserve, entities := seedEntities()

	if err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", len(entities), testWait); err != nil {
		t.Fatalf("seedAll: %v", err)
	}
	for id, looks := range transactions.looks {
		if looks != 3 {
			t.Errorf("seed %s looked at %d times, want 3: until it was seen completed", id, looks)
		}
	}
	if len(transactions.looks) != len(entities) {
		t.Errorf("looked at %d seeds, want %d", len(transactions.looks), len(entities))
	}
}

// openSeeds counts seeds started but not yet seen completed, keeping the most
// that were ever open at once.
type openSeeds struct {
	*scriptedTransactions

	completed map[string]bool
	open      int
	mostOpen  int
}

func (f *openSeeds) StartInitializingTransaction(
	ctx context.Context, req *transactionpb.StartInitializingTransactionRequest,
) (*transactionpb.StartInitializingTransactionResponse, error) {
	f.open++
	f.mostOpen = max(f.mostOpen, f.open)
	return f.scriptedTransactions.StartInitializingTransaction(ctx, req)
}

func (f *openSeeds) GetTransactionState(
	ctx context.Context, req *transactionpb.GetTransactionStateRequest,
) (*transactionpb.GetTransactionStateResponse, error) {
	resp, err := f.scriptedTransactions.GetTransactionState(ctx, req)
	if err == nil && resp.GetState() == txCompleted && !f.completed[req.GetId()] {
		f.completed[req.GetId()] = true
		f.open--
	}
	return resp, err
}

// Every seed debits the one reserve Wallet, so seeds that prepare side by side
// race on its stream. seedAll settles them a batch at a time, never starting
// the next batch until the last has committed, so no more than a batch ever
// races at once however many entities there are.
func TestSeedAll_SettlesAtMostOneBatchAtATime(t *testing.T) {
	t.Parallel()
	transactions := &openSeeds{
		scriptedTransactions: newScriptedTransactions(txInitialized, txCompleted),
		completed:            map[string]bool{},
	}
	reserve := entity{name: "reserve", walletID: "reserve"}
	entities := make([]entity, 5)
	for i := range entities {
		entities[i] = entity{name: fmt.Sprintf("entity-%d", i), walletID: fmt.Sprintf("w%d", i)}
	}

	if err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", 2, testWait); err != nil {
		t.Fatalf("seedAll: %v", err)
	}
	if transactions.mostOpen != 2 {
		t.Errorf("at most %d seeds were open at once, want 2: one batch", transactions.mostOpen)
	}
	if len(transactions.completed) != len(entities) {
		t.Errorf("%d seeds completed, want %d: every entity funded", len(transactions.completed), len(entities))
	}
}

func TestSeedAll_FailsWhenASeedEndsWithoutCompleting(t *testing.T) {
	t.Parallel()
	transactions := newScriptedTransactions(txStarted, txRolledBack)
	reserve, entities := seedEntities()

	err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", len(entities), testWait)
	if err == nil || !strings.Contains(err.Error(), "TRANSACTION_STATE_ROLLED_BACK") {
		t.Errorf("seedAll error = %v, want one naming the state the seed ended in", err)
	}
}

// The likeliest reason no seed ever completes is that nothing is folding them,
// so the error says where to look.
func TestSeedAll_FailsPointingAtTheOrchestratorWhenASeedNeverMoves(t *testing.T) {
	t.Parallel()
	transactions := newScriptedTransactions(txInitialized)
	reserve, entities := seedEntities()

	err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", len(entities), settleWait{attempts: 2})
	if err == nil || !strings.Contains(err.Error(), "orchestrator") {
		t.Errorf("seedAll error = %v, want one pointing at go/cmd/orchestrator", err)
	}
}

func TestSeedAll_FailsOnARejectedSeed(t *testing.T) {
	t.Parallel()
	transactions := newScriptedTransactions(txInitialized)
	transactions.rejectAs = "reserve wallet not found"
	reserve, entities := seedEntities()

	err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", len(entities), testWait)
	if err == nil || !strings.Contains(err.Error(), "reserve wallet not found") {
		t.Errorf("seedAll error = %v, want the rejection's reason", err)
	}
}
