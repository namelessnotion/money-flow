package transfer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// contendedWalletStore lands a genuine competing write on walletID — another
// Token minted into it — just before each of the first n AppendAtomic calls,
// the way a sibling Transfer into the same Wallet prepares a moment earlier on
// another partition. The call itself then reaches the real store with the
// Wallet's now-stale ExpectedSeq, and loses for real.
type contendedWalletStore struct {
	eventstore.Store
	t        *testing.T
	lc       ledger.Client
	walletID string
	n        int
	landed   int
}

func (s *contendedWalletStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	if s.landed < s.n {
		s.landed++
		mintToken(s.t, s.Store, s.lc, s.walletID, testutil.ID(fmt.Sprintf("sibling-%d", s.landed)), usd(1))
	}
	return s.Store.AppendAtomic(ctx, writes...)
}

func acceptedTransferInto(t *testing.T, store eventstore.Store, lc ledger.Client) (from, to, transferID string) {
	t.Helper()
	from, to, transferID = testutil.ID("w1"), testutil.ID("w2"), testutil.ID("xfer1")
	openWallet(t, store, from, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, to, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintToken(t, store, lc, from, testutil.ID("t1"), usd(1000))
	fundToken(t, lc, testutil.ID("t1"), 1000)

	resp, err := NewServer(store, lc, nil, nil).RequestTransfer(context.Background(),
		transferRequest(transferID, from, to, usd(400), true))
	if err != nil || resp.GetTransferRequestAccepted() == nil {
		t.Fatalf("RequestTransfer() = %v, %v; want Accepted", resp.GetResult(), err)
	}
	return from, to, transferID
}

// Losing the destination Wallet to a sibling Transfer is contention, not a
// fault: the Tokens this Transfer mints are still its own to mint, only the
// Wallet's position moved. Prepare re-plans against the Wallet as it now
// stands rather than handing the orchestrator an error to back off on — and,
// under enough of it, halt over (go/docs/adr/0003).
func TestPrepare_ReplansWhenASiblingTransferMovesTheDestinationWallet(t *testing.T) {
	t.Parallel()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	_, to, transferID := acceptedTransferInto(t, store, lc)

	contended := &contendedWalletStore{Store: store, t: t, lc: lc, walletID: to, n: 2}
	if err := NewServer(contended, lc, nil, nil).Resume(context.Background(), transferID); err != nil {
		t.Fatalf("Resume() error = %v, want prepare to re-plan past two siblings landing first", err)
	}
	if state := currentState(mustEvents(t, store, transferID)); state != stateStaged {
		t.Errorf("state = %v, want staged", state)
	}
}

// Re-planning is bounded: a Wallet that moves under every attempt is reported
// back to the driver rather than retried forever, and nothing half-written is
// left behind.
func TestPrepare_GivesUpOnAWalletThatNeverStopsMoving(t *testing.T) {
	t.Parallel()
	store, lc := eventstore.NewMemoryStore(), ledger.NewFakeClient()
	_, to, transferID := acceptedTransferInto(t, store, lc)

	contended := &contendedWalletStore{Store: store, t: t, lc: lc, walletID: to, n: maxPrepareAttempts}
	err := NewServer(contended, lc, nil, nil).Resume(context.Background(), transferID)
	if err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("Resume() error = %v, want prepare to give up after %d attempts", err, maxPrepareAttempts)
	}
	if contended.landed != maxPrepareAttempts {
		t.Errorf("prepare tried %d times, want %d", contended.landed, maxPrepareAttempts)
	}
	if state := currentState(mustEvents(t, store, transferID)); state != stateAccepted {
		t.Errorf("state = %v, want still accepted", state)
	}
}
