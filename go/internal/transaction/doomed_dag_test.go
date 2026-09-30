package transaction

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// These tests replay the doomed Transactions spec/alloy/dag.als finds: DAGs
// that validateDAG and the funding pre-flight accept (ADR 0004), but that
// can't complete in any order, because some leg's source can never cover it
// when it runs. The model proves no order completes. These show that go/
// accepts each one and that it rolls back. They pin today's behaviour: if the
// pre-flight learns to refuse one of these classes, its case should expect
// TransactionRejected instead.

type doomedWallet struct {
	allows sharedpb.Allows
	funded uint64 // an existing, spendable balance; 0 for none
}

type doomedLeg struct {
	from, to string
	amount   uint64
	mint     bool
	after    []string
}

func TestDoomedTransactionsAreAcceptedAndRollBack(t *testing.T) {
	none := sharedpb.Allows_ALLOWS_NONE
	both := sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP

	tests := []struct {
		name    string
		wallets map[string]doomedWallet
		legs    map[string]doomedLeg
	}{
		{
			// CreditsOnrampWallet: the destination Token is minted with
			// credits_must_not_exceed_debits, so the credit is refused.
			name: "a credit into an onramp wallet",
			wallets: map[string]doomedWallet{
				"w":    {allows: none, funded: 100},
				"card": {allows: sharedpb.Allows_ALLOWS_ONRAMP},
			},
			legs: map[string]doomedLeg{
				"a": {from: "w", to: "card", amount: 100},
			},
		},
		{
			// MintFromUnmintableWallet: the pre-flight skips mint_source roots,
			// and validateMintSource refuses this one at dispatch.
			name: "a mint_source leg from a wallet that doesn't allow onramp",
			wallets: map[string]doomedWallet{
				"w": {allows: none},
				"x": {allows: none},
			},
			legs: map[string]doomedLeg{
				"a": {from: "w", to: "x", amount: 100, mint: true},
			},
		},
		{
			// NetOverdrawn: each root is covered on its own, not both.
			name: "two roots that together overdraw one wallet",
			wallets: map[string]doomedWallet{
				"w": {allows: none, funded: 100},
				"x": {allows: none},
				"y": {allows: none},
			},
			legs: map[string]doomedLeg{
				"a": {from: "w", to: "x", amount: 100},
				"b": {from: "w", to: "y", amount: 100},
			},
		},
		{
			// UnfundableLeg, dag.als's FundedOnlyDownstream: a's source is
			// only ever paid by b, which runs after a.
			name: "a leg funded only by its own descendant",
			wallets: map[string]doomedWallet{
				"z": {allows: none, funded: 100},
				"q": {allows: none},
				"w": {allows: none},
				"x": {allows: none},
			},
			legs: map[string]doomedLeg{
				"r": {from: "z", to: "q", amount: 100},
				"a": {from: "w", to: "x", amount: 100, after: []string{"r"}},
				"b": {from: "q", to: "w", amount: 100, after: []string{"a"}},
			},
		},
		{
			// RefillWaitsOnDebits: both roots spend all of w, and the refill
			// waits for both.
			name: "a refill that waits on two debits which together overdraw",
			wallets: map[string]doomedWallet{
				"w":    {allows: none, funded: 300},
				"v":    {allows: none},
				"u":    {allows: none},
				"bank": {allows: both},
			},
			legs: map[string]doomedLeg{
				"a": {from: "w", to: "v", amount: 300},
				"b": {from: "w", to: "u", amount: 300},
				"c": {from: "bank", to: "w", amount: 300, mint: true, after: []string{"a", "b"}},
			},
		},
		{
			// dag.als's RefillFundedThroughAnotherWallet, which none of the
			// static classes catch: c refills w after a, and is funded only
			// by b, so whichever root runs second finds w short.
			name: "a refill funded through another wallet",
			wallets: map[string]doomedWallet{
				"w": {allows: none, funded: 300},
				"v": {allows: none},
				"u": {allows: none},
			},
			legs: map[string]doomedLeg{
				"a": {from: "w", to: "v", amount: 200},
				"b": {from: "w", to: "u", amount: 200},
				"c": {from: "u", to: "w", amount: 200, after: []string{"a"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := eventstore.NewMemoryStore()
			lc := ledger.NewFakeClient()

			walletIDs := make(map[string]string, len(tt.wallets))
			for name, w := range tt.wallets {
				walletIDs[name] = testutil.ID("wallet-" + name)
				openWallet(t, store, walletIDs[name], w.allows)
				if w.funded > 0 {
					mintAndFundToken(t, store, lc, walletIDs[name], testutil.ID("token-"+name), usd(w.funded))
				}
			}

			transfers := make(map[string]*pb.Transfer, len(tt.legs))
			deps := map[string]*pb.TransferIdList{}
			legIDs := make(map[string]string, len(tt.legs))
			for name := range tt.legs {
				legIDs[name] = testutil.ID("leg-" + name)
			}
			for name, leg := range tt.legs {
				id := legIDs[name]
				transfers[id] = &pb.Transfer{
					Id: id, Amount: usd(leg.amount), MintSource: leg.mint,
					FromWalletId: walletIDs[leg.from], ToWalletId: walletIDs[leg.to],
				}
				if len(leg.after) > 0 {
					parents := make([]string, len(leg.after))
					for i, parent := range leg.after {
						parents[i] = legIDs[parent]
					}
					deps[id] = &pb.TransferIdList{TransferId: parents}
				}
			}

			xfers := newTransferServer(store, lc)
			txns := NewServer(store, xfers)
			txnID := testutil.ID("doomed")
			resp, err := txns.StartInitializingTransaction(ctx, &pb.StartInitializingTransactionRequest{
				Id: txnID, Transfers: transfers, TransferDependency: deps,
			})
			if err != nil {
				t.Fatalf("StartInitializingTransaction() error = %v", err)
			}
			if resp.GetTransactionInitialized() == nil {
				t.Fatalf("StartInitializingTransaction() = %v, want TransactionInitialized", resp.GetResult())
			}

			driveSaga(t, txns, xfers, store, txnID)

			events, err := store.Load(ctx, AggregateType, txnID)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got := topLevelState(events); got != stateRolledBack {
				t.Fatalf("state = %v, want rolled_back; events = %v", got, eventTypesOf(events))
			}
		})
	}
}
