package transaction

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// achWallets provisions the four Wallets the ACH deposit worked example
// needs: Bank Account and Bank Control both get ALLOWS_ONRAMP_AND_OFFRAMP
// (decision #11 — Bank Control must support both a mint_source debit on
// deposit and an ordinary credit on withdrawal); Cash and Uncleared get
// ALLOWS_NONE, matching every non-funding-instrument Wallet in Ruby's own
// AccountType#allows mapping.
func achWallets(t *testing.T, store eventstore.Store) (bankAccount, cash, bankControl, uncleared string) {
	t.Helper()
	bankAccount = testutil.ID("bank-account")
	cash = testutil.ID("cash")
	bankControl = testutil.ID("bank-control")
	uncleared = testutil.ID("uncleared")
	openWallet(t, store, bankAccount, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, cash, sharedpb.Allows_ALLOWS_NONE)
	openWallet(t, store, bankControl, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, uncleared, sharedpb.Allows_ALLOWS_NONE)
	return bankAccount, cash, bankControl, uncleared
}

func TestACHDeposit_RealAndShadowHappyPath(t *testing.T) {
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

	resp, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			realID: {
				Id: realID, Amount: usd(10000), FromWalletId: bankAccount, ToWalletId: cash,
				AutoProcess: true, Stage: true, MintSource: true,
			},
			shadowID: {
				Id: shadowID, Amount: usd(10000), FromWalletId: bankControl, ToWalletId: uncleared,
				AutoProcess: true, Stage: false, MintSource: true,
			},
		},
		TransferDependency: map[string]*pb.TransferIdList{
			shadowID: {TransferId: []string{realID}},
		},
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	if resp.GetTransactionInitialized() == nil {
		t.Fatalf("result = %v, want TransactionInitialized", resp.GetResult())
	}

	// real dispatched and staged (ACH-style); shadow must not be touched yet
	// — it depends on real reaching Completed, and real hasn't posted.
	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if children[realID] != childRequested {
		t.Fatalf("real child state = %v, want requested", children[realID])
	}
	if _, touched := children[shadowID]; touched {
		t.Fatalf("shadow child state = %v, want untouched (depends on real completing)", children[shadowID])
	}
	if outcome, err := transfer.Outcome(ctx, store, realID); err != nil || outcome != transfer.OutcomeStaged {
		t.Fatalf("real Outcome() = (%v, %v), want OutcomeStaged", outcome, err)
	}

	// ...1-3 real days pass, ACH settles...
	if _, err := xferServer.ConfirmStagedTransfer(ctx, &transferpb.ConfirmStagedTransferRequest{Id: realID}); err != nil {
		t.Fatalf("ConfirmStagedTransfer() error = %v", err)
	}
	if _, err := xferServer.PostPendingTransfer(ctx, &transferpb.PostPendingTransferRequest{Id: realID}); err != nil {
		t.Fatalf("PostPendingTransfer() error = %v", err)
	}

	// Ruby calls ResumeTransaction once it's told real posted; shadow must
	// fire automatically (auto_process=true, no gating) and commit
	// synchronously since it's unstaged, completing the whole Transaction.
	resumeResp, err := txnServer.ResumeTransaction(ctx, &pb.ResumeTransactionRequest{Id: txnID})
	if err != nil {
		t.Fatalf("ResumeTransaction() error = %v", err)
	}
	if resumeResp.GetState() != pb.TransactionState_TRANSACTION_STATE_COMPLETED {
		t.Fatalf("state = %v, want COMPLETED", resumeResp.GetState())
	}

	if outcome, err := transfer.Outcome(ctx, store, shadowID); err != nil || outcome != transfer.OutcomeCommitted {
		t.Fatalf("shadow Outcome() = (%v, %v), want OutcomeCommitted", outcome, err)
	}
}

func TestTransaction2_SingleNodeClearingHappyPath(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	uncleared := testutil.ID("uncleared")
	cleared := testutil.ID("cleared")
	openWallet(t, store, uncleared, sharedpb.Allows_ALLOWS_NONE)
	openWallet(t, store, cleared, sharedpb.Allows_ALLOWS_NONE)
	mintAndFundToken(t, store, lc, uncleared, testutil.ID("uncleared-token"), usd(10000))

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn2")
	clearID := testutil.ID("clear")
	resp, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			clearID: {Id: clearID, Amount: usd(10000), FromWalletId: uncleared, ToWalletId: cleared, AutoProcess: true},
		},
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}
	if resp.GetTransactionInitialized() == nil {
		t.Fatalf("result = %v, want TransactionInitialized", resp.GetResult())
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if topLevelState(events) != stateCompleted {
		t.Fatalf("state = %v, want completed in one runSaga pass (single-node, ordinary FIFO, no gating)", topLevelState(events))
	}
}

// TestACHWithdrawal_SymmetricFlowNeedsNoNewMechanism proves decision #12:
// the withdrawal direction (real: Cash -> Bank Account; shadow: Cleared ->
// Bank Control) is just a differently-shaped DAG of ordinary Transfers —
// neither leg needs mint_source, since Cash and Cleared already hold real
// balance from prior deposits/clearing. The only thing this depends on
// that the deposit direction didn't exercise is Bank Control's own
// ALLOWS_ONRAMP_AND_OFFRAMP policy (decision #11) accepting its first-ever
// credit without a TigerBeetle rejection.
//
// This test's dependency direction (shadow depends on real) predates
// ruby/docs/adr/0004: Ruby's TransactionShape now sends the opposite
// wiring (real depends on shadow — funding happens first) so an
// underfunded withdrawal is caught before the real leg ever reaches the
// provider. server_test.go's achWithdrawalDAG/TestStartInitializingTransaction_*
// tests use that current (v2) shape. This test is left as-is: it still
// legitimately proves the DAG mechanism is symmetric regardless of
// direction, it's just no longer literally what Ruby configures today.
// The new accept-time pre-check (go/docs/adr/0004, transaction package) is
// a no-op here either way, since both legs are funded.
func TestACHWithdrawal_SymmetricFlowNeedsNoNewMechanism(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, bankControl, _ := achWallets(t, store)
	cleared := testutil.ID("cleared")
	openWallet(t, store, cleared, sharedpb.Allows_ALLOWS_NONE)

	// Cash and Cleared already hold real balance from prior deposits/clearing
	// — exactly what a withdrawal's ordinary FIFO selection needs, unlike
	// the deposit direction's fresh mint_source Tokens.
	mintAndFundToken(t, store, lc, cash, testutil.ID("cash-token"), usd(10000))
	mintAndFundToken(t, store, lc, cleared, testutil.ID("cleared-token"), usd(10000))

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn-withdrawal")
	realID := testutil.ID("real-withdrawal")
	shadowID := testutil.ID("shadow-withdrawal")
	_, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			// Neither leg sets MintSource — both source Wallets already
			// hold real, FIFO-selectable balance.
			realID:   {Id: realID, Amount: usd(10000), FromWalletId: cash, ToWalletId: bankAccount, AutoProcess: true},
			shadowID: {Id: shadowID, Amount: usd(10000), FromWalletId: cleared, ToWalletId: bankControl, AutoProcess: true},
		},
		TransferDependency: map[string]*pb.TransferIdList{
			shadowID: {TransferId: []string{realID}},
		},
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if topLevelState(events) != stateCompleted {
		t.Fatalf("state = %v, want completed; events = %v", topLevelState(events), eventTypesOf(events))
	}
	if outcome, err := transfer.Outcome(ctx, store, shadowID); err != nil || outcome != transfer.OutcomeCommitted {
		t.Fatalf("shadow Outcome() = (%v, %v), want OutcomeCommitted — Bank Control must accept its first-ever credit", outcome, err)
	}
}

func TestGenericManualGating_GatedChildWaitsForStartProcessingTransfer(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	w1, w2 := testutil.ID("w1"), testutil.ID("w2")
	openWallet(t, store, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintAndFundToken(t, store, lc, w1, testutil.ID("t1"), usd(1000))

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	rootID := testutil.ID("root")
	gatedID := testutil.ID("gated")
	_, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id: txnID,
		Transfers: map[string]*pb.Transfer{
			rootID:  {Id: rootID, Amount: usd(100), FromWalletId: w1, ToWalletId: w2, AutoProcess: true},
			gatedID: {Id: gatedID, Amount: usd(50), FromWalletId: w1, ToWalletId: w2, AutoProcess: false},
		},
		TransferDependency: map[string]*pb.TransferIdList{
			gatedID: {TransferId: []string{rootID}},
		},
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	if children[rootID] != childCompleted {
		t.Fatalf("root child state = %v, want completed", children[rootID])
	}
	if children[gatedID] != childGated {
		t.Fatalf("gated child state = %v, want gated (auto_process=false, dependency satisfied but not triggered)", children[gatedID])
	}
	if topLevelState(events) != stateStarted {
		t.Fatalf("top-level state = %v, want started (still waiting on StartProcessingTransfer)", topLevelState(events))
	}

	// Resuming without triggering it changes nothing — still gated.
	if _, err := txnServer.ResumeTransaction(ctx, &pb.ResumeTransactionRequest{Id: txnID}); err != nil {
		t.Fatalf("ResumeTransaction() error = %v", err)
	}
	events, _ = store.Load(ctx, AggregateType, txnID)
	children, _ = foldChildStates(events)
	if children[gatedID] != childGated {
		t.Fatalf("gated child state after ResumeTransaction = %v, want still gated", children[gatedID])
	}

	processResp, err := txnServer.StartProcessingTransfer(ctx, &pb.StartProcessingTransferRequest{Id: txnID, TransferId: gatedID})
	if err != nil {
		t.Fatalf("StartProcessingTransfer() error = %v", err)
	}
	if processResp.GetTransferRequestedWithinTransaction() == nil {
		t.Fatalf("result = %v, want TransferRequestedWithinTransaction", processResp.GetResult())
	}

	events, err = store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if topLevelState(events) != stateCompleted {
		t.Fatalf("top-level state = %v, want completed once the gated child is triggered and commits", topLevelState(events))
	}
}

func TestStartProcessingTransfer_RejectsWhenNotGated(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	w1, w2 := testutil.ID("w1"), testutil.ID("w2")
	openWallet(t, store, w1, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	openWallet(t, store, w2, sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP)
	mintAndFundToken(t, store, lc, w1, testutil.ID("t1"), usd(1000))

	xferServer := newTransferServer(store, lc)
	txnServer := NewServer(store, xferServer)
	ctx := context.Background()

	txnID := testutil.ID("txn1")
	rootID := testutil.ID("root")
	if _, err := txnServer.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
		Id:        txnID,
		Transfers: map[string]*pb.Transfer{rootID: {Id: rootID, Amount: usd(100), FromWalletId: w1, ToWalletId: w2, AutoProcess: true}},
	}); err != nil {
		t.Fatalf("StartInitializingTransaction() error = %v", err)
	}

	// rootID already ran to completion (auto_process=true) — not gated.
	resp, err := txnServer.StartProcessingTransfer(ctx, &pb.StartProcessingTransferRequest{Id: txnID, TransferId: rootID})
	if err != nil {
		t.Fatalf("StartProcessingTransfer() error = %v", err)
	}
	if resp.GetStartProcessingTransferRejected() == nil {
		t.Fatalf("result = %v, want StartProcessingTransferRejected", resp.GetResult())
	}
}
