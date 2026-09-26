package transfer

import (
	"context"
	"strings"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// acceptedThenDrained accepts a Transfer of 100 out of a wallet holding 100,
// then spends that 100 elsewhere before the Transfer is prepared: what a
// concurrent Transaction debiting the same wallet does in the gap between
// accept and prepare (namelessnotion/money_flow#6).
func acceptedThenDrained(t *testing.T) (store eventstore.Store, lc ledger.Client, transferID string) {
	t.Helper()
	ctx := context.Background()
	store = eventstore.NewMemoryStore()
	lc = ledger.NewFakeClient()
	from, to := testutil.ID("w1"), testutil.ID("w2")
	openWallet(t, store, from, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, from, testutil.ID("t1"), usd(100))
	fundToken(t, lc, testutil.ID("t1"), 100)

	transferID = testutil.ID("xfer1")
	resp, err := NewServer(store, lc, nil, nil).RequestTransfer(ctx, transferRequest(transferID, from, to, usd(100), false))
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("RequestTransfer() = %v, want accepted while the wallet holds 100", resp.GetResult())
	}

	sink := ledger.Account{ID: testutil.ID("elsewhere"), Currency: "USD"}
	if _, err := lc.CreateAccounts(ctx, []ledger.Account{sink}); err != nil {
		t.Fatalf("create sink account: %v", err)
	}
	results, err := lc.CreateTransfers(ctx, []ledger.Transfer{{
		ID: testutil.ID("spent-elsewhere"), DebitAccountID: testutil.ID("t1"), CreditAccountID: sink.ID,
		MinorUnits: 100, Currency: "USD", Kind: ledger.TransferKindRegular,
	}})
	if err != nil || results[0].Result != ledger.TransferResultOK {
		t.Fatalf("drain the wallet: results = %v, err = %v", results, err)
	}
	return store, lc, transferID
}

// The money is no longer there, which is a domain outcome like any ledger
// refusal: the Transfer fails, and its Transaction rolls back. It must not
// be an error, which the orchestrator retries and then halts on (go ADR 0003).
func TestPrepare_ALegItsWalletCanNoLongerCoverFails(t *testing.T) {
	t.Parallel()
	store, lc, transferID := acceptedThenDrained(t)
	ctx := context.Background()

	if err := NewServer(store, lc, nil, nil).Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume() error = %v, want the shortfall recorded as a failure", err)
	}

	if got, err := Outcome(ctx, store, transferID); err != nil || got != OutcomeFailed {
		t.Fatalf("Outcome() = %v, %v; want failed", got, err)
	}
	events := mustEvents(t, store, transferID)
	msg, err := events[len(events)-1].Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	failed, ok := msg.(*pb.TransferFailed)
	if !ok {
		t.Fatalf("last event = %T, want *TransferFailed", msg)
	}
	if !strings.Contains(failed.GetReason(), "insufficient Token capacity") {
		t.Errorf("reason = %q, want it to name the shortfall", failed.GetReason())
	}
	for _, e := range events {
		if e.EventType == eventstore.EventType(&pb.TransferPrepared{}) {
			t.Errorf("stream records TransferPrepared; a Transfer that can't be covered is never prepared")
		}
	}

	// Resuming a failed Transfer again is a no-op, as for any terminal.
	if err := NewServer(store, lc, nil, nil).Resume(ctx, transferID); err != nil {
		t.Errorf("second Resume() error = %v, want nil", err)
	}
	if n := len(mustEvents(t, store, transferID)); n != len(events) {
		t.Errorf("second Resume() appended %d events, want none", n-len(events))
	}
}

// The failure is recorded against the stream exactly as prepare loaded it. If
// anything lands on it first, here a cancel, the failure must not be
// written on top of it. Re-deciding from the new state is what keeps it
// from ever failing a Transfer that another caller has already moved on.
func TestPrepare_AShortfallNeverOverwritesAConcurrentOutcome(t *testing.T) {
	t.Parallel()
	base, lc, transferID := acceptedThenDrained(t)
	ctx := context.Background()

	racing := &conflictNTimesStore{Store: base, n: 1, onConflict: func() {
		if err := base.Append(ctx, AggregateType, transferID, 1,
			&pb.AcceptedTransferCancelled{Id: transferID, Reason: "cancelled concurrently"}); err != nil {
			t.Errorf("append the concurrent cancel: %v", err)
		}
	}}

	if err := NewServer(racing, lc, nil, nil).Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	if got := lastEventType(t, base, transferID); got != eventstore.EventType(&pb.AcceptedTransferCancelled{}) {
		t.Errorf("last event = %s, want the concurrent cancel to stand", got)
	}
	for _, e := range mustEvents(t, base, transferID) {
		if e.EventType == eventstore.EventType(&pb.TransferFailed{}) {
			t.Errorf("TransferFailed was recorded on top of the concurrent cancel")
		}
	}
}
