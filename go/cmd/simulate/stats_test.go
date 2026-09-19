package main

import (
	"errors"
	"testing"
	"time"
)

func TestSummarizeCountsByFinalStateAndErrors(t *testing.T) {
	t.Parallel()

	results := []txResult{
		{final: "TRANSACTION_STATE_COMPLETED", moved: true, latency: 10 * time.Millisecond},
		{final: "TRANSACTION_STATE_COMPLETED", moved: true, latency: 20 * time.Millisecond},
		{final: "TRANSACTION_STATE_ROLLED_BACK", latency: 30 * time.Millisecond},
		{err: errors.New("boom")},
	}

	s := summarize(results, time.Second)

	if s.total != 4 {
		t.Errorf("total = %d, want 4", s.total)
	}
	if s.errors != 1 {
		t.Errorf("errors = %d, want 1", s.errors)
	}
	if got := s.byState["TRANSACTION_STATE_COMPLETED"]; got != 2 {
		t.Errorf("byState[COMPLETED] = %d, want 2", got)
	}
	if got := s.byState["TRANSACTION_STATE_ROLLED_BACK"]; got != 1 {
		t.Errorf("byState[ROLLED_BACK] = %d, want 1", got)
	}
	if got := s.byState["rpc_error"]; got != 1 {
		t.Errorf("byState[rpc_error] = %d, want 1", got)
	}
	if s.throughput != 4 {
		t.Errorf("throughput = %v, want 4/sec", s.throughput)
	}
}

func TestSummarizeCountsOpenAcrossModes(t *testing.T) {
	t.Parallel()

	results := []txResult{
		{final: "TRANSACTION_STATE_STARTED", open: true},
		{final: "TRANSFER_ACCEPTED", open: true},
		{final: "TRANSFER_COMMITTED", moved: true},
	}

	s := summarize(results, time.Second)

	if s.openCount != 2 {
		t.Errorf("openCount = %d, want 2", s.openCount)
	}
}

func TestPercentileIsExactAtTheEdges(t *testing.T) {
	t.Parallel()

	sorted := []time.Duration{1, 2, 3, 4, 5}
	if got := percentile(sorted, 0); got != 1 {
		t.Errorf("percentile(0) = %v, want 1", got)
	}
	if got := percentile(sorted, 1); got != 5 {
		t.Errorf("percentile(1) = %v, want 5", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
}

func TestComputeExpectedOnlyAppliesCompletedTransactions(t *testing.T) {
	t.Parallel()

	initial := map[string]int64{"a": 1000, "b": 1000}
	results := []txResult{
		{fromWallet: "a", toWallet: "b", amountMinor: 100, final: "TRANSACTION_STATE_COMPLETED", moved: true},
		{fromWallet: "a", toWallet: "b", amountMinor: 500, final: "TRANSACTION_STATE_ROLLED_BACK"},
		{fromWallet: "b", toWallet: "a", amountMinor: 50, final: "TRANSACTION_STATE_COMPLETED", moved: true},
		{fromWallet: "a", toWallet: "b", amountMinor: 999, err: errors.New("timeout")},
	}

	got := computeExpected(initial, results)

	if got["a"] != 950 {
		t.Errorf("expected[a] = %d, want 950", got["a"])
	}
	if got["b"] != 1050 {
		t.Errorf("expected[b] = %d, want 1050", got["b"])
	}
}
