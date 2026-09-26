package transaction

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

// These tests replay, step by step, the race spec/alloy/ledger.als found
// against FundedWithdrawalCanBePaid: a Borrower's Repayment and ACH
// withdrawal, started together against one dollar that is both cleared and
// cash. Each Transaction debits the two sides with separate legs (ruby ADR
// 0009, ADR 0004). When the Repayment ran its cash leg beside its money leg,
// it could take the cash after the withdrawal's Funding had taken the cleared
// cash, and a funded withdrawal was refused (namelessnotion/money_flow#7).
//
// Both shapes now debit the cleared cash first (ruby ADR 0010), so whichever
// Transaction takes the cleared cash has the cash behind it. These tests run
// both orders the Borrower's cleared cash can be taken in.

// twoSidesWorld is one Borrower's money on both sides, a Security's Repayment
// wallets, and the bank boundary: every Wallet the two shapes touch.
type twoSidesWorld struct {
	store                                          eventstore.Store
	lc                                             *ledger.FakeClient
	xfers                                          *transfer.Server
	txns                                           *Server
	cleared, cash, bankControl, bankAccount        string
	securityRepayment, securityCash                string
	withdrawalID, withdrawalReal, withdrawalShadow string
	repaymentID, repaymentMoney, repaymentCash     string
}

func newTwoSidesWorld(t *testing.T, amount uint64) *twoSidesWorld {
	t.Helper()
	w := &twoSidesWorld{store: eventstore.NewMemoryStore(), lc: ledger.NewFakeClient()}
	w.bankAccount, w.cash, w.bankControl, _ = achWallets(t, w.store)
	w.cleared = testutil.ID("cleared")
	w.securityRepayment = testutil.ID("security-repayment")
	w.securityCash = testutil.ID("security-cash")
	for _, id := range []string{w.cleared, w.securityRepayment, w.securityCash} {
		openWallet(t, w.store, id, sharedpb.Allows_ALLOWS_NONE)
	}
	// The two sides agree: the Borrower holds `amount`, seen twice.
	mintAndFundToken(t, w.store, w.lc, w.cleared, testutil.ID("cleared-token"), usd(amount))
	mintAndFundToken(t, w.store, w.lc, w.cash, testutil.ID("cash-token"), usd(amount))

	w.xfers = newTransferServer(w.store, w.lc)
	w.txns = NewServer(w.store, w.xfers)

	w.withdrawalID = testutil.ID("withdrawal")
	w.withdrawalReal, w.withdrawalShadow = testutil.ID("withdrawal-real"), testutil.ID("withdrawal-shadow")
	transfers, deps := achWithdrawalDAG(w.cash, w.bankAccount, w.cleared, w.bankControl,
		w.withdrawalReal, w.withdrawalShadow, usd(amount))
	w.start(t, w.withdrawalID, transfers, deps)

	// security_repayment v3 (ruby/app/services/securities/repayment_shape.rb):
	// a money leg, and its cash leg after it.
	w.repaymentID = testutil.ID("repayment")
	w.repaymentMoney, w.repaymentCash = testutil.ID("repayment-money"), testutil.ID("repayment-cash")
	w.start(t, w.repaymentID, map[string]*pb.Transfer{
		w.repaymentMoney: {Id: w.repaymentMoney, Amount: usd(amount), FromWalletId: w.cleared, ToWalletId: w.securityRepayment},
		w.repaymentCash:  {Id: w.repaymentCash, Amount: usd(amount), FromWalletId: w.cash, ToWalletId: w.securityCash},
	}, map[string]*pb.TransferIdList{w.repaymentCash: {TransferId: []string{w.repaymentMoney}}})
	return w
}

// start asks Go to accept a Transaction and requires that it did: at time
// zero each one is fully funded on its own, so Go's accept-time pre-flight
// (go ADR 0004) passes both.
func (w *twoSidesWorld) start(t *testing.T, id string, transfers map[string]*pb.Transfer, deps map[string]*pb.TransferIdList) {
	t.Helper()
	resp, err := w.txns.StartInitializingTransaction(context.Background(), &pb.StartInitializingTransactionRequest{
		Id: id, Transfers: transfers, TransferDependency: deps,
	})
	if err != nil {
		t.Fatalf("StartInitializingTransaction(%s) error = %v", id, err)
	}
	if resp.GetTransactionInitialized() == nil {
		t.Fatalf("StartInitializingTransaction(%s) = %v, want TransactionInitialized", id, resp.GetResult())
	}
}

// dispatch resumes the Transaction once, which requests its ready children
// without moving any money: each is left Accepted for its own saga.
func (w *twoSidesWorld) dispatch(t *testing.T, transactionID string) {
	t.Helper()
	if err := w.txns.Resume(context.Background(), transactionID); err != nil {
		t.Fatalf("Resume(%s) error = %v", transactionID, err)
	}
}

// commit runs one child Transfer's own saga, and requires that it committed.
func (w *twoSidesWorld) commit(t *testing.T, transferID string) {
	t.Helper()
	ctx := context.Background()
	if err := w.xfers.Resume(ctx, transferID); err != nil {
		t.Fatalf("Resume(transfer %s) error = %v", transferID, err)
	}
	if got := w.outcome(t, transferID); got != transfer.OutcomeCommitted {
		t.Fatalf("transfer %s outcome = %v, want committed", transferID, got)
	}
}

func (w *twoSidesWorld) outcome(t *testing.T, transferID string) transfer.OutcomeKind {
	t.Helper()
	got, err := transfer.Outcome(context.Background(), w.store, transferID)
	if err != nil {
		t.Fatalf("Outcome(%s) error = %v", transferID, err)
	}
	return got
}

func (w *twoSidesWorld) state(t *testing.T, transactionID string) transactionState {
	t.Helper()
	events, err := w.store.Load(context.Background(), AggregateType, transactionID)
	if err != nil {
		t.Fatalf("Load(%s) error = %v", transactionID, err)
	}
	return topLevelState(events)
}

// posted is a Wallet's posted balance: the sum over every Token minted into
// it, reversals' included.
func (w *twoSidesWorld) posted(t *testing.T, walletID string) int64 {
	t.Helper()
	ctx := context.Background()
	tokens, err := wallet.TokensOf(ctx, w.store, walletID)
	if err != nil {
		t.Fatalf("TokensOf(%s) error = %v", walletID, err)
	}
	balances, err := w.lc.Balances(ctx, tokens)
	if err != nil {
		t.Fatalf("Balances(%s) error = %v", walletID, err)
	}
	var total int64
	for _, b := range balances {
		net, err := b.PostedNet()
		if err != nil {
			t.Fatalf("PostedNet() error = %v", err)
		}
		total += net
	}
	return total
}

// The Repayment takes the cleared cash first. The withdrawal's Funding is
// then refused, so the withdrawal rolls back without ever having been funded,
// and the Repayment's cash leg finds the cash its money leg left behind. When
// the cash leg ran beside the money leg, it could take the cash first, and
// a withdrawal funded meanwhile was refused (namelessnotion/money_flow#7).
func TestTwoSidesRace_ARepaymentThatTakesTheClearedCashFirstLeavesTheWithdrawalUnfunded(t *testing.T) {
	t.Parallel()
	const amount = 10000
	w := newTwoSidesWorld(t, amount)

	w.dispatch(t, w.repaymentID)
	if got := w.outcome(t, w.repaymentCash); got != transfer.OutcomeNotFound {
		t.Fatalf("repayment cash leg outcome = %v, want not requested until its money leg commits", got)
	}
	w.commit(t, w.repaymentMoney) // cleared: amount -> 0, cash untouched

	driveSaga(t, w.txns, w.xfers, w.store, w.withdrawalID)
	if got := w.outcome(t, w.withdrawalShadow); got != transfer.OutcomeRejected {
		t.Errorf("withdrawal Funding outcome = %v, want rejected: the Repayment took the cleared cash", got)
	}
	if got := w.state(t, w.withdrawalID); got != stateRolledBack {
		t.Fatalf("withdrawal = %v, want rolled_back, never funded", got)
	}

	driveSaga(t, w.txns, w.xfers, w.store, w.repaymentID)
	if got := w.state(t, w.repaymentID); got != stateCompleted {
		t.Fatalf("repayment = %v, want completed", got)
	}

	for name, tc := range map[string]struct {
		walletID string
		want     int64
	}{
		"cleared":            {w.cleared, 0},
		"cash":               {w.cash, 0},
		"security_repayment": {w.securityRepayment, amount},
		"security_cash":      {w.securityCash, amount},
	} {
		if got := w.posted(t, tc.walletID); got != tc.want {
			t.Errorf("%s posted = %d, want %d", name, got, tc.want)
		}
	}
}

// The withdrawal takes the cleared cash first, after the Repayment's money
// leg has been accepted. When that leg is prepared, its re-selection finds
// nothing, so the leg fails and the Repayment rolls back. Its cash leg is
// never requested, and the withdrawal's real leg finds the cash it was
// funded for. Before namelessnotion/money_flow#6 the prepare returned an
// error on every retry instead, and the orchestrator halted on it (go ADR
// 0003).
func TestTwoSidesRace_ALegAcceptedBeforeItsWalletIsDrainedFailsAndRollsBack(t *testing.T) {
	t.Parallel()
	const amount = 10000
	w := newTwoSidesWorld(t, amount)

	w.dispatch(t, w.repaymentID) // the Repayment's money leg Accepted, nothing moved

	w.dispatch(t, w.withdrawalID)
	w.commit(t, w.withdrawalShadow) // Funding: cleared amount -> 0

	if err := w.xfers.Resume(context.Background(), w.repaymentMoney); err != nil {
		t.Fatalf("Resume(repayment money leg) error = %v, want the shortfall recorded as a failure", err)
	}
	if got := w.outcome(t, w.repaymentMoney); got != transfer.OutcomeFailed {
		t.Errorf("repayment money leg outcome = %v, want failed", got)
	}

	driveSaga(t, w.txns, w.xfers, w.store, w.repaymentID)
	if got := w.state(t, w.repaymentID); got != stateRolledBack {
		t.Fatalf("repayment = %v, want rolled_back", got)
	}
	if got := w.outcome(t, w.repaymentCash); got != transfer.OutcomeNotFound {
		t.Errorf("repayment cash leg outcome = %v, want never requested", got)
	}

	driveSaga(t, w.txns, w.xfers, w.store, w.withdrawalID)
	if got := w.outcome(t, w.withdrawalReal); got != transfer.OutcomeStaged {
		t.Errorf("withdrawal real leg outcome = %v, want staged: a funded withdrawal is paid", got)
	}

	// Only the withdrawal's Funding moved money. The Borrower's cash is still
	// there for the withdrawal's real leg.
	for name, tc := range map[string]struct {
		walletID string
		want     int64
	}{
		"cleared":            {w.cleared, 0},
		"cash":               {w.cash, amount},
		"security_repayment": {w.securityRepayment, 0},
		"security_cash":      {w.securityCash, 0},
	} {
		if got := w.posted(t, tc.walletID); got != tc.want {
			t.Errorf("%s posted = %d, want %d", name, got, tc.want)
		}
	}
}
