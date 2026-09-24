package transaction

import (
	"context"
	"strings"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

func usd(minorUnits uint64) *sharedpb.Money {
	return &sharedpb.Money{MinorUnits: minorUnits, Currency: "USD"}
}

// mintSourceChildDAG builds a single-root, mint_source DAG — enough to
// exercise StartInitializingTransaction's own accept/reject/idempotency
// without needing a working transferClient, since a mint_source child has no
// balance to pre-check and so never reaches into it (see
// wouldAcceptReadyChildren).
func mintSourceChildDAG(childID string) map[string]*pb.Transfer {
	return map[string]*pb.Transfer{
		childID: {
			Id: childID, Amount: usd(400),
			FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"),
			MintSource: true,
		},
	}
}

func TestStartInitializingTransaction_Accepts(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	client := newAcceptingTransferClient()
	server := NewServer(store, client)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	childID := testutil.ID("xfer1")
	resp, err := server.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID, Transfers: mintSourceChildDAG(childID),
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	if resp.GetTransactionInitialized() == nil {
		t.Fatalf("result = %v, want TransactionInitialized", resp.GetResult())
	}

	// Accepting records one event and stops. TransactionStarted is the saga's
	// to write, from the trigger this acceptance publishes, so a Transaction
	// that has only been accepted really is Initialized and nothing more —
	// asserting the stream is exactly that one event is what would catch
	// dispatch creeping back into the handler.
	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("stream holds %d events, want exactly the one TransactionInitialized; types = %v",
			len(events), eventTypesOf(events))
	}
	if topLevelState(events) != stateInitialized {
		t.Fatalf("state = %v, want initialized", topLevelState(events))
	}
	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if len(children) != 0 {
		t.Errorf("children = %v, want none touched yet", children)
	}
	if len(client.requested) != 0 {
		t.Errorf("requested %v while accepting, want nothing requested until the saga is driven", client.requested)
	}

	// Driving it is what requests the child.
	driveSaga(t, server, nil, store, txnID)
	events, err = store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	children, err = foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if children[childID] != childRequested {
		t.Errorf("child state = %v, want requested", children[childID])
	}
	if client.requested[childID] == 0 {
		t.Errorf("child %s was never requested of the Transfer aggregate", childID)
	}
}

func TestStartInitializingTransaction_RejectsCyclicDAG(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	childID := testutil.ID("xfer1")
	spec := mintSourceChildDAG(childID)[childID]
	txnID := testutil.ID("txn1")
	resp, err := server.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id:        txnID,
		Transfers: map[string]*pb.Transfer{childID: spec},
		TransferDependency: map[string]*pb.TransferIdList{
			childID: {TransferId: []string{childID}}, // self-dependency
		},
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	if resp.GetTransactionRejected() == nil {
		t.Fatalf("result = %v, want TransactionRejected", resp.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("stream holds %d events, want 1 (the rejection itself)", len(events))
	}
	if events[0].EventType != "transaction.v1.TransactionRejected" {
		t.Errorf("recorded event type = %q, want TransactionRejected", events[0].EventType)
	}
}

func TestStartInitializingTransaction_RejectsEmptyTransfers(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	resp, err := server.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{Id: testutil.ID("txn1")})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	if resp.GetTransactionRejected() == nil {
		t.Fatalf("result = %v, want TransactionRejected", resp.GetResult())
	}
}

// A leg that moves nothing used to be accepted here and only refused later,
// at dispatch, as a twirp error nobody sees — leaving the Transaction in
// Started with nothing on its stream to explain why it stopped
// (ruby/docs/adr/0006). It is now one TransactionRejected and no dispatch at
// all. Asserting the stream is exactly that one event is the half that would
// catch the check drifting to after dispatch.
func TestStartInitializingTransaction_RejectsAZeroAmountLegBeforeDispatchingAnything(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	// A nil transferClient is the assertion: reaching dispatch would panic.
	server := NewServer(store, nil)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	childID := testutil.ID("xfer1")
	resp, err := server.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{childID: {
			Id: childID, Amount: usd(0),
			FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"),
		}},
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	rejected := resp.GetTransactionRejected()
	if rejected == nil {
		t.Fatalf("result = %v, want TransactionRejected", resp.GetResult())
	}
	if !strings.Contains(rejected.GetReason(), "amount") {
		t.Errorf("reason = %q, want it to name the amount as the problem", rejected.GetReason())
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("stream has %d events, want exactly the one TransactionRejected", len(events))
	}
	if topLevelState(events) != stateRejected {
		t.Errorf("state = %v, want rejected", topLevelState(events))
	}
}

func TestStartInitializingTransaction_IsIdempotent(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	req := &pb.StartInitializingTransactionRequest{Id: txnID, Transfers: mintSourceChildDAG(testutil.ID("xfer1"))}
	first, err := server.StartInitializingTransaction(ctx, req)
	if err != nil {
		t.Fatalf("first StartInitializingTransaction() error = %v", err)
	}
	second, err := server.StartInitializingTransaction(ctx, req)
	if err != nil {
		t.Fatalf("second StartInitializingTransaction() error = %v", err)
	}
	if first.GetTransactionInitialized().GetId() != second.GetTransactionInitialized().GetId() {
		t.Errorf("replay result = %v, want the same recorded outcome", second.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// Just the one TransactionInitialized: accepting writes nothing else, and
	// the replay adds nothing either.
	if len(events) != 1 {
		t.Errorf("stream holds %d events, want 1 (no duplicate work on replay); types = %v", len(events), eventTypesOf(events))
	}
}

func TestStartInitializingTransaction_RejectedRequestIsIdempotent(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	req := &pb.StartInitializingTransactionRequest{Id: txnID} // empty transfers -> rejected
	if _, err := server.StartInitializingTransaction(ctx, req); err != nil {
		t.Fatalf("first StartInitializingTransaction() error = %v", err)
	}
	second, err := server.StartInitializingTransaction(ctx, req)
	if err != nil {
		t.Fatalf("second StartInitializingTransaction() error = %v", err)
	}
	if second.GetTransactionRejected() == nil {
		t.Fatalf("result = %v, want TransactionRejected on replay", second.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Errorf("stream holds %d events, want 1 (no duplicate rejection on replay)", len(events))
	}
}

func TestIsOpen_InitializedAndStartedAreOpen(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	if _, err := server.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID, Transfers: mintSourceChildDAG(testutil.ID("xfer1")),
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	open, err := IsOpen(ctx, store, txnID)
	if err != nil {
		t.Fatalf("IsOpen() error = %v", err)
	}
	if !open {
		t.Error("IsOpen() = false, want true (Started is an open state)")
	}
}

func TestIsOpen_UnknownTransactionDefaultsOpen(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()

	open, err := IsOpen(context.Background(), store, testutil.ID("never-existed"))
	if err != nil {
		t.Fatalf("IsOpen() error = %v", err)
	}
	if !open {
		t.Error("IsOpen() = false, want true (defensive default: fail toward blocking on a not-found stream)")
	}
}

func TestExists_UnknownTransactionIsFalse(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()

	exists, err := Exists(context.Background(), store, testutil.ID("never-existed"))
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if exists {
		t.Error("Exists() = true, want false (opposite default from IsOpen: a claimed id must resolve to a real stream)")
	}
}

func TestExists_InitializedTransactionIsTrue(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	if _, err := server.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID, Transfers: mintSourceChildDAG(testutil.ID("xfer1")),
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	exists, err := Exists(ctx, store, txnID)
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if !exists {
		t.Error("Exists() = false, want true")
	}
}

// achWithdrawalDAG builds the ADR-0004 (v2) withdrawal shape exactly as
// ruby/app/services/ach/transaction_shape.rb wires it: the shadow leg
// (Cleared -> Bank Control, funding, no mint_source) is the DAG root — the
// one wouldAcceptReadyChildren must pre-check — and the real leg (Cash ->
// Bank Account, staged) depends on it, unlike
// TestACHWithdrawal_SymmetricFlowNeedsNoNewMechanism's older, pre-ADR-0004
// shape (real first, shadow depends on real).
func achWithdrawalDAG(cash, bankAccount, cleared, bankControl, realID, shadowID string, amount *sharedpb.Money) (
	map[string]*pb.Transfer, map[string]*pb.TransferIdList,
) {
	return map[string]*pb.Transfer{
			realID:   {Id: realID, Amount: amount, FromWalletId: cash, ToWalletId: bankAccount, Stage: true},
			shadowID: {Id: shadowID, Amount: amount, FromWalletId: cleared, ToWalletId: bankControl},
		}, map[string]*pb.TransferIdList{
			realID: {TransferId: []string{shadowID}},
		}
}

func TestStartInitializingTransaction_RejectsUnderfundedReadyChild(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, bankControl, _ := achWallets(t, store)
	cleared := testutil.ID("cleared")
	openWallet(t, store, cleared, sharedpb.Allows_ALLOWS_NONE)
	// Cash is funded (the real leg would be fine); Cleared — the shadow
	// leg's own source, and the DAG root — is not funded at all.
	mintAndFundToken(t, store, lc, cash, testutil.ID("cash-token"), usd(10000))

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn-withdrawal")
	realID, shadowID := testutil.ID("real-withdrawal"), testutil.ID("shadow-withdrawal")
	transfers, deps := achWithdrawalDAG(cash, bankAccount, cleared, bankControl, realID, shadowID, usd(10000))
	resp, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID, Transfers: transfers, TransferDependency: deps,
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	rejected := resp.GetTransactionRejected()
	if rejected == nil {
		t.Fatalf("result = %v, want TransactionRejected", resp.GetResult())
	}
	if !strings.Contains(rejected.GetReason(), shadowID) || !strings.Contains(rejected.GetReason(), "insufficient Token capacity") {
		t.Errorf("reason = %q, want it to name %q and the underlying shortfall", rejected.GetReason(), shadowID)
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("stream holds %d events, want 1 (the rejection itself — no Initialized/Started/dispatch/rollback cascade); types = %v",
			len(events), eventTypesOf(events))
	}
}

func TestStartInitializingTransaction_AcceptsFundedReadyChild(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, bankControl, _ := achWallets(t, store)
	cleared := testutil.ID("cleared")
	openWallet(t, store, cleared, sharedpb.Allows_ALLOWS_NONE)
	mintAndFundToken(t, store, lc, cash, testutil.ID("cash-token"), usd(10000))
	mintAndFundToken(t, store, lc, cleared, testutil.ID("cleared-token"), usd(10000))

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn-withdrawal")
	realID, shadowID := testutil.ID("real-withdrawal"), testutil.ID("shadow-withdrawal")
	transfers, deps := achWithdrawalDAG(cash, bankAccount, cleared, bankControl, realID, shadowID, usd(10000))
	resp, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID, Transfers: transfers, TransferDependency: deps,
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	if resp.GetTransactionInitialized() == nil {
		t.Fatalf("result = %v, want TransactionInitialized", resp.GetResult())
	}

	// The pre-check passing is additive, not a replacement: the real dispatch
	// must still actually run, and now it runs under the trigger loop rather
	// than inside this call — shadow (unstaged) commits, unblocking the staged
	// real leg, which then waits for an external confirmation.
	driveSaga(t, txnServer, xferServer, store, txnID)

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if children[shadowID] != childCompleted {
		t.Fatalf("shadow child state = %v, want completed (the real dispatch ran, not just the pre-check)", children[shadowID])
	}
	if children[realID] != childRequested {
		t.Fatalf("real child state = %v, want requested (staged, waiting on external confirmation)", children[realID])
	}
}

// TestStartInitializingTransaction_SkipsMintSourceReadyChild proves the
// mint_source exclusion is load-bearing, not cosmetic: this DAG's root
// child mints its own source Token and has NO funded token behind its
// wallet at all — if wouldAcceptReadyChildren didn't skip mint_source
// children, this would spuriously reject via TransactionExistsChecker
// (this Transaction doesn't exist yet) even though mint_source has no
// balance dimension to be underfunded on in the first place.
func TestStartInitializingTransaction_SkipsMintSourceReadyChild(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, _, _ := achWallets(t, store)

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn-deposit")
	realID := testutil.ID("real-deposit")
	resp, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			realID: {
				Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash,
				Stage: true, MintSource: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	if resp.GetTransactionInitialized() == nil {
		t.Fatalf("result = %v, want TransactionInitialized (mint_source has no balance to be underfunded on)", resp.GetResult())
	}
}

func eventTypesOf(events []eventstore.Event) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.EventType
	}
	return types
}
