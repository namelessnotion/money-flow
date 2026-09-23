package main

import (
	"context"
	"slices"
	"testing"

	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

func newTransferDriver(leg *scriptedLeg) (transferDriver, *recordingTransfers) {
	transfers := &recordingTransfers{leg: leg}
	return transferDriver{transfers: transfers, leg: leg.read, wait: testWait}, transfers
}

func driveTransfer(d transferDriver, planned outcome) txResult {
	return d.drive(context.Background(), entity{walletID: "from"}, entity{walletID: "to"}, 500, "USD", planned)
}

// RequestTransfer only accepts. Confirming a Transfer the orchestrator has not
// staged yet is refused, and that refusal is written to the Transfer's own
// stream — so the tool must not ask until it has seen the leg staged.
func TestTransferDriver_ConfirmsOnlyOnceStaged(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeInFlight, transfer.OutcomeInFlight, transfer.OutcomeStaged, transfer.OutcomeCommitted)
	d, transfers := newTransferDriver(leg)

	r := driveTransfer(d, outcomeComplete)

	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if want := []string{"confirm at staged", "post at staged"}; !slices.Equal(transfers.calls, want) {
		t.Errorf("settlement calls = %q, want %q", transfers.calls, want)
	}
	if r.final != "TRANSFER_COMMITTED" || !r.completed() || r.open {
		t.Errorf("result = %s completed=%v open=%v, want TRANSFER_COMMITTED, completed, closed", r.final, r.completed(), r.open)
	}
}

func TestTransferDriver_CancelsOnlyOnceStaged(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeInFlight, transfer.OutcomeStaged, transfer.OutcomeCancelled)
	d, transfers := newTransferDriver(leg)

	r := driveTransfer(d, outcomeRollback)

	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if want := []string{"cancel at staged"}; !slices.Equal(transfers.calls, want) {
		t.Errorf("settlement calls = %q, want %q", transfers.calls, want)
	}
	if r.final != "TRANSFER_CANCELLED" || r.completed() || r.open {
		t.Errorf("result = %s completed=%v open=%v, want TRANSFER_CANCELLED, not completed, closed", r.final, r.completed(), r.open)
	}
}

func TestTransferDriver_ReportsARequestTimeRejectionWithoutWaiting(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeNotFound)
	d, transfers := newTransferDriver(leg)
	transfers.rejectAs = "insufficient funds"

	r := driveTransfer(d, outcomeComplete)

	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if r.final != "TRANSFER_REJECTED" || r.reason != "insufficient funds" || r.completed() || r.stuck() {
		t.Errorf("result = %s (%q), want TRANSFER_REJECTED (insufficient funds), neither completed nor stuck", r.final, r.reason)
	}
	if got := leg.totalLooks(); got != 0 {
		t.Errorf("leg looked at %d times, want 0", got)
	}
}

// A Transfer that fails before staging — losing on funds once it actually
// prepares — has nothing left for the tool to decide.
func TestTransferDriver_LeavesATransferThatFailedOnItsOwnAlone(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeInFlight, transfer.OutcomeFailed)
	d, transfers := newTransferDriver(leg)

	r := driveTransfer(d, outcomeComplete)

	if len(transfers.calls) != 0 {
		t.Errorf("settlement calls = %q, want none", transfers.calls)
	}
	if r.final != "TRANSFER_FAILED" || r.completed() || r.open {
		t.Errorf("result = %s completed=%v open=%v, want TRANSFER_FAILED, not completed, closed", r.final, r.completed(), r.open)
	}
}

func TestTransferDriver_GivesUpStillOpenAndUnsettled(t *testing.T) {
	t.Parallel()
	leg := newScriptedLeg(transfer.OutcomeInFlight)
	d, transfers := newTransferDriver(leg)
	d.wait = settleWait{attempts: 2}

	r := driveTransfer(d, outcomeComplete)

	if !r.open || r.final != "TRANSFER_IN_FLIGHT" {
		t.Errorf("result = %s open=%v, want TRANSFER_IN_FLIGHT and still open", r.final, r.open)
	}
	if len(transfers.calls) != 0 {
		t.Errorf("settlement calls = %q, want none", transfers.calls)
	}
}
