package transaction

import (
	"context"
	"sync"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// TestMidDAGFailure_TriggersReverseTopologicalRollback mirrors the plan's
// worked rollback example: real (Bank Account -> Cash, mint_source) commits
// first; shadow (a never-provisioned Bank Control Wallet -> Uncleared,
// mint_source) is rejected at accept time (mint_source skips the balance
// check, so a not-found Wallet is the realistic failure here). Rollback
// must trigger automatically and reverse real. Shadow moved no money, so its
// failure is all there is to record about it: nothing rolls it back
// (go/docs/adr/0013).
func TestMidDAGFailure_TriggersReverseTopologicalRollback(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount := testutil.ID("bank-account")
	cash := testutil.ID("cash")
	uncleared := testutil.ID("uncleared")
	neverProvisionedBankControl := testutil.ID("bank-control-not-provisioned")
	openWallet(t, store, bankAccount, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, cash, sharedpb.Allows_ALLOWS_NONE)
	openWallet(t, store, uncleared, sharedpb.Allows_ALLOWS_NONE)
	// neverProvisionedBankControl is deliberately never opened.

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	realID := testutil.ID("real")
	shadowID := testutil.ID("shadow")
	if _, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			realID:   {Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true},
			shadowID: {Id: shadowID, Amount: usd(10000), FromWalletId: neverProvisionedBankControl, ToWalletId: uncleared, MintSource: true},
		},
		TransferDependency: map[string]*pb.TransferIdList{
			shadowID: {TransferId: []string{realID}},
		},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	driveSaga(t, txnServer, xferServer, store, txnID)

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if topLevelState(events) != stateRolledBack {
		t.Fatalf("state = %v, want rolled_back", topLevelState(events))
	}

	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if children[shadowID] != childFailed {
		t.Fatalf("shadow child state = %v, want failed: it moved no money, so nothing rolls it back", children[shadowID])
	}
	if children[realID] != childRolledBack {
		t.Fatalf("real child state = %v, want rolled_back", children[realID])
	}

	realMethod, realReversalID := rolledBackMethod(t, events, realID)
	if realMethod != pb.RollbackMethod_ROLLBACK_METHOD_REVERSED {
		t.Errorf("real rollback method = %v, want REVERSED", realMethod)
	}
	if realReversalID == "" {
		t.Error("real rollback detail_id is empty, want the reversal Transfer's own id")
	}
	if outcome, err := transfer.Outcome(ctx, store, realReversalID); err != nil || outcome != transfer.OutcomeCommitted {
		t.Fatalf("reversal Outcome() = (%v, %v), want OutcomeCommitted", outcome, err)
	}
}

// rolledBackMethod scans events for transferID's TransferRolledBackWithinTransaction
// and returns its method and detail_id.
func rolledBackMethod(t *testing.T, events []eventstore.Event, transferID string) (pb.RollbackMethod, string) {
	t.Helper()
	for _, e := range events {
		if e.EventType != eventstore.EventType(&pb.TransferRolledBackWithinTransaction{}) {
			continue
		}
		msg, err := e.Decode()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		rb, ok := msg.(*pb.TransferRolledBackWithinTransaction)
		if ok && rb.GetTransferId() == transferID {
			return rb.GetMethod(), rb.GetDetailId()
		}
	}
	t.Fatalf("no TransferRolledBackWithinTransaction found for %q", transferID)
	return pb.RollbackMethod_ROLLBACK_METHOD_UNSPECIFIED, ""
}

// rejectingAfterNCallsClient wraps a real ledger.Client but forces every
// CreateTransfers call from the (n+1)th onward to report a rejection —
// used to simulate a TigerBeetle-level rejection of a specific later
// transfer (here, a rollback's own reversal) without also poisoning an
// earlier, legitimate commit.
type rejectingAfterNCallsClient struct {
	ledger.Client
	mu    sync.Mutex
	calls int
	n     int
}

func (r *rejectingAfterNCallsClient) CreateTransfers(ctx context.Context, transfers []ledger.Transfer) ([]ledger.TransferResult, error) {
	r.mu.Lock()
	r.calls++
	reject := r.calls > r.n
	r.mu.Unlock()
	if !reject {
		return r.Client.CreateTransfers(ctx, transfers)
	}
	results := make([]ledger.TransferResult, len(transfers))
	for i := range transfers {
		results[i] = ledger.TransferResult{Index: i, Result: ledger.TransferResultExceedsCredits}
	}
	return results, nil
}

// TestRollbackFailure_ReachesTransactionRollbackFailed forces the
// reversal's own TigerBeetle submission (not its accept-time decision) to
// be rejected — the "rollback itself can fail" case decision #4 exists
// for — and confirms the saga lands on TransactionRollbackFailed, not
// TransactionRolledBack, while still resolving every other unblocked
// child.
func TestRollbackFailure_ReachesTransactionRollbackFailed(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	realLC := ledger.NewFakeClient()
	// Call #1: real's own mint_source debit batch (must succeed). Every
	// call after that (the eventual reversal's own commit) is rejected.
	lc := &rejectingAfterNCallsClient{Client: realLC, n: 1}

	bankAccount := testutil.ID("bank-account")
	cash := testutil.ID("cash")
	openWallet(t, store, bankAccount, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, cash, sharedpb.Allows_ALLOWS_NONE)

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	realID := testutil.ID("real")
	failID := testutil.ID("fail")
	// failID targets a wallet that was never opened, so it fails at accept
	// time — no CreateTransfers call, no extra call consumed against n.
	neverOpened := testutil.ID("never-opened")
	if _, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			realID: {Id: realID, Amount: usd(1000), FromWalletId: bankAccount, ToWalletId: cash, MintSource: true},
			failID: {Id: failID, Amount: usd(1000), FromWalletId: neverOpened, ToWalletId: cash, MintSource: true},
		},
		TransferDependency: map[string]*pb.TransferIdList{
			failID: {TransferId: []string{realID}},
		},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	driveSaga(t, txnServer, xferServer, store, txnID)

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if topLevelState(events) != stateRollbackFailed {
		t.Fatalf("state = %v, want rollback_failed; events = %v", topLevelState(events), eventTypesOf(events))
	}

	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if children[failID] != childFailed {
		t.Errorf("fail child state = %v, want failed (resolved by its own failure, despite the other child being stuck)", children[failID])
	}
	if children[realID] != childRollbackFailed {
		t.Errorf("real child state = %v, want rollback_failed", children[realID])
	}
}

// TestCrossTransactionTokenReservation_HidesThenRevealsCashToken proves the
// narrow transient-rejection window called out under Key design points is
// real, bounded, and resolves itself: while Transaction A is still open
// (real committed but shadow hasn't finished), a concurrent standalone
// Transfer against the same Cash Wallet is rejected for insufficient
// capacity; once A completes, the identical request succeeds.
func TestCrossTransactionTokenReservation_HidesThenRevealsCashToken(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, bankControl, uncleared := achWallets(t, store)
	outsideWallet := testutil.ID("outside")
	openWallet(t, store, outsideWallet, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txnA")
	realID := testutil.ID("real")
	shadowID := testutil.ID("shadow")
	if _, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			realID: {
				Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash,
				Stage: true, MintSource: true,
			},
			shadowID: {
				Id: shadowID, Amount: usd(10000), FromWalletId: bankControl, ToWalletId: uncleared,
				MintSource: true,
			},
		},
		TransferDependency: map[string]*pb.TransferIdList{shadowID: {TransferId: []string{realID}}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	driveSaga(t, txnServer, xferServer, store, txnID)
	// Settle real (ACH posts) but do NOT resume the Transaction yet — shadow
	// stays undispatched, so Transaction A is still open.
	if _, err := xferServer.ConfirmStagedTransfer(ctx, &transferpb.ConfirmStagedTransferRequest{Id: realID}); err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if _, err := xferServer.PostPendingTransfer(ctx, &transferpb.PostPendingTransferRequest{Id: realID}); err != nil {
		t.Fatalf("PostPendingTransfer() error = %v", err)
	}

	// A standalone Transfer (no transaction_id) tries to spend Cash's now-
	// posted balance while A is still open.
	standaloneID := testutil.ID("standalone")
	resp, err := xferServer.RequestTransfer(ctx, &transferpb.RequestTransferRequest{
		Id: standaloneID, FromWalletId: cash, ToWalletId: outsideWallet, Amount: usd(5000),
	})
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if resp.GetTransferRequestRejected() == nil {
		t.Fatalf("result = %v, want TransferRequestRejected while Transaction A is still open", resp.GetResult())
	}

	// Now let A finish: posting the real leg published a trigger, and folding
	// it fires the shadow leg and completes the Transaction.
	driveSaga(t, txnServer, xferServer, store, txnID)
	events, _ := store.Load(ctx, AggregateType, txnID)
	if topLevelState(events) != stateCompleted {
		t.Fatalf("Transaction A state = %v, want completed", topLevelState(events))
	}

	// The identical standalone request now succeeds.
	standaloneID2 := testutil.ID("standalone2")
	resp2, err := xferServer.RequestTransfer(ctx, &transferpb.RequestTransferRequest{
		Id: standaloneID2, FromWalletId: cash, ToWalletId: outsideWallet, Amount: usd(5000),
	})
	if err != nil {
		t.Fatalf("RequestTransfer() error = %v", err)
	}
	if resp2.GetTransferRequestAccepted() == nil {
		t.Fatalf("result = %v, want TransferRequestAccepted once Transaction A has closed", resp2.GetResult())
	}
}

// TestSameTransactionVisibility_DAGCanSpendItsOwnTaggedToken uses a
// synthetic three-node DAG (Transaction 1's own two children don't happen
// to share a Token dependency — shadow's source is Bank Control, unrelated
// to real's Cash output) to prove the tag only ever blocks OTHER
// Transactions, never the owning DAG's own internal chaining: A mints a
// destination Token (tagged with this Transaction's id), and B — a second
// child of the SAME Transaction, depending on A — successfully spends that
// same tagged Token as its own source.
func TestSameTransactionVisibility_DAGCanSpendItsOwnTaggedToken(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount := testutil.ID("bank-account")
	middle := testutil.ID("middle")
	final := testutil.ID("final")
	openWallet(t, store, bankAccount, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, middle, sharedpb.Allows_ALLOWS_NONE)
	openWallet(t, store, final, sharedpb.Allows_ALLOWS_NONE)

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	aID := testutil.ID("a")
	bID := testutil.ID("b")
	if _, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			aID: {Id: aID, Amount: usd(1000), FromWalletId: bankAccount, ToWalletId: middle, MintSource: true},
			bID: {Id: bID, Amount: usd(1000), FromWalletId: middle, ToWalletId: final},
		},
		TransferDependency: map[string]*pb.TransferIdList{bID: {TransferId: []string{aID}}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	driveSaga(t, txnServer, xferServer, store, txnID)

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if topLevelState(events) != stateCompleted {
		t.Fatalf("state = %v, want completed (b must be able to spend a's tagged output)", topLevelState(events))
	}
	if outcome, err := transfer.Outcome(ctx, store, bID); err != nil || outcome != transfer.OutcomeCommitted {
		t.Fatalf("b Outcome() = (%v, %v), want OutcomeCommitted", outcome, err)
	}
}

// TestRunSaga_IdempotentMidDAGResume mirrors Transfer's own resumability
// tests: calling runSaga (via Resume) after only partial
// progress is recorded converges to the same state a full run would reach,
// and an extra call afterward is a pure no-op.
func TestRunSaga_IdempotentMidDAGResume(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, bankControl, uncleared := achWallets(t, store)

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	realID := testutil.ID("real")
	shadowID := testutil.ID("shadow")
	if _, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			realID:   {Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash, Stage: true, MintSource: true},
			shadowID: {Id: shadowID, Amount: usd(10000), FromWalletId: bankControl, ToWalletId: uncleared, MintSource: true},
		},
		TransferDependency: map[string]*pb.TransferIdList{shadowID: {TransferId: []string{realID}}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	driveSaga(t, txnServer, xferServer, store, txnID)

	// Stuck mid-DAG: real is only Staged, nothing more can happen yet.
	eventsBefore, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if topLevelState(eventsBefore) != stateStarted {
		t.Fatalf("state = %v, want started (stuck waiting on real's staged settlement)", topLevelState(eventsBefore))
	}

	// Resuming before anything external happened converges to the exact
	// same state — a pure no-op.
	if err := txnServer.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	eventsAfter, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("event count after a no-progress resume = %d, want unchanged from %d", len(eventsAfter), len(eventsBefore))
	}

	// Now let real settle, and drive again — should reach Completed.
	if _, err := xferServer.ConfirmStagedTransfer(ctx, &transferpb.ConfirmStagedTransferRequest{Id: realID}); err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if _, err := xferServer.PostPendingTransfer(ctx, &transferpb.PostPendingTransferRequest{Id: realID}); err != nil {
		t.Fatalf("PostPendingTransfer() error = %v", err)
	}
	driveSaga(t, txnServer, xferServer, store, txnID)
	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if topLevelState(events) != stateCompleted {
		t.Fatalf("state = %v, want completed", topLevelState(events))
	}

	// One more resume on an already-terminal Transaction is a pure no-op.
	countBefore := len(events)
	if err := txnServer.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	events, err = store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != countBefore {
		t.Errorf("event count after resuming a terminal Transaction = %d, want unchanged from %d", len(events), countBefore)
	}
}
