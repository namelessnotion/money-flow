package main

import (
	"context"
	"strings"
	"testing"
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

	if err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", testWait); err != nil {
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

func TestSeedAll_FailsWhenASeedEndsWithoutCompleting(t *testing.T) {
	t.Parallel()
	transactions := newScriptedTransactions(txStarted, txRolledBack)
	reserve, entities := seedEntities()

	err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", testWait)
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

	err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", settleWait{attempts: 2})
	if err == nil || !strings.Contains(err.Error(), "orchestrator") {
		t.Errorf("seedAll error = %v, want one pointing at go/cmd/orchestrator", err)
	}
}

func TestSeedAll_FailsOnARejectedSeed(t *testing.T) {
	t.Parallel()
	transactions := newScriptedTransactions(txInitialized)
	transactions.rejectAs = "reserve wallet not found"
	reserve, entities := seedEntities()

	err := seedAll(context.Background(), transactions, reserve, entities, 1000, "USD", testWait)
	if err == nil || !strings.Contains(err.Error(), "reserve wallet not found") {
		t.Errorf("seedAll error = %v, want the rejection's reason", err)
	}
}
