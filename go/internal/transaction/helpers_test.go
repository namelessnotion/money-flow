package transaction

import (
	"context"
	"testing"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	walletpb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/token"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

// newTransferServer wires a real *transfer.Server to this package's own
// IsOpen/Exists — the same wiring cmd/server/main.go does — so end-to-end
// tests exercise the real cross-transaction Token-reservation and
// mint_source-guarantee mechanisms, not a fake.
func newTransferServer(store eventstore.Store, lc ledger.Client) *transfer.Server {
	isOpen := func(ctx context.Context, transactionID string) (bool, error) {
		return IsOpen(ctx, store, transactionID)
	}
	exists := func(ctx context.Context, transactionID string) (bool, error) {
		return Exists(ctx, store, transactionID)
	}
	return transfer.NewServer(store, lc, isOpen, exists)
}

// acceptingTransferClient accepts every RequestTransfer and writes nothing,
// for tests about what the Transaction records rather than what its children
// do. It never creates a Transfer stream, so a child it accepted stays an
// intent with no Transfer (transfer.OutcomeNotFound), and the next fold
// completes that request again — harmlessly, since this accepts again. Any
// other transferClient method is the nil embedded interface's, and panics:
// a test that reached one needed a real transfer.Server.
type acceptingTransferClient struct {
	transferClient
	requested map[string]int
}

func newAcceptingTransferClient() *acceptingTransferClient {
	return &acceptingTransferClient{requested: map[string]int{}}
}

func (c *acceptingTransferClient) RequestTransfer(_ context.Context, req *transferpb.RequestTransferRequest) (*transferpb.RequestTransferResponse, error) {
	c.requested[req.GetId()]++
	return &transferpb.RequestTransferResponse{
		Id: req.GetId(),
		Result: &transferpb.RequestTransferResponse_TransferRequestAccepted{
			TransferRequestAccepted: &transferpb.TransferRequestAccepted{Id: req.GetId(), TransactionId: req.GetTransactionId()},
		},
	}, nil
}

func openWallet(t *testing.T, store eventstore.Store, walletID string, allows sharedpb.Allows) {
	t.Helper()
	if _, err := wallet.NewServer(store).Open(context.Background(), &walletpb.OpenRequest{
		Id: walletID, HolderId: testutil.ID("h1"), Name: "test wallet", Allows: allows,
	}); err != nil {
		t.Fatalf("open wallet: %v", err)
	}
}

// mintAndFundToken opens (if needed) and mints a Token of the given
// capacity into walletID, then gives it a matching posted TigerBeetle
// balance by transferring in from a throwaway external account — for
// wallets that need ordinary, FIFO-selectable pre-existing balance (Cash,
// Uncleared, Cleared), unlike the mint_source wallets (Bank Account, Bank
// Control) whose Tokens are minted fresh by the Transfer saga itself.
func mintAndFundToken(t *testing.T, store eventstore.Store, lc ledger.Client, walletID, tokenID string, capacity *sharedpb.Money) {
	t.Helper()
	ctx := context.Background()
	ts := token.NewServer(store, lc)
	resp, err := ts.Mint(ctx, &tokenpb.MintRequest{Id: tokenID, WalletId: walletID, Capacity: capacity})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if resp.GetTokenMintRejected() != nil {
		t.Fatalf("mint token rejected: %v", resp.GetTokenMintRejected())
	}

	source := ledger.Account{ID: testutil.ID("external-funding-" + tokenID), Currency: capacity.GetCurrency()}
	if _, err := lc.CreateAccounts(ctx, []ledger.Account{source}); err != nil {
		t.Fatalf("fund token: create external account: %v", err)
	}
	xfer := ledger.Transfer{
		ID: testutil.ID("fund-" + tokenID), DebitAccountID: source.ID, CreditAccountID: tokenID,
		MinorUnits: capacity.GetMinorUnits(), Currency: capacity.GetCurrency(), Kind: ledger.TransferKindRegular,
	}
	results, err := lc.CreateTransfers(ctx, []ledger.Transfer{xfer})
	if err != nil {
		t.Fatalf("fund token: transfer: %v", err)
	}
	if results[0].Result != ledger.TransferResultOK {
		t.Fatalf("fund token: transfer result = %v, want OK", results[0].Result)
	}
}

// driveSaga stands in for cmd/orchestrator. Nothing in the RPC surface
// advances a saga any more, so a test that wants a Transaction to get
// anywhere has to say what drove it — which is the point, and is why this is
// called explicitly at each site rather than hidden inside a wrapper around
// StartInitializingTransaction.
//
// One round resumes the Transaction, then every child Transfer it has heard
// of, then the Transaction again — the same two-step the real orchestrator
// makes when a transfer-topic trigger arrives and it follows the link to the
// owning Transaction (saga.Orchestrator.handleTransfer). Rounds repeat until
// one of them appends nothing anywhere, which is the quiescence a trigger loop
// reaches when there is no further event to deliver.
//
// xfers may be nil for a Transaction driven through a fake transferClient,
// where there are no real child Transfer streams to resume.
//
// It returns how many rounds it took, so a test can assert that slicing really
// did spread the work across more than one.
func driveSaga(t *testing.T, ts *Server, xfers *transfer.Server, store eventstore.Store, transactionID string) int {
	t.Helper()
	ctx := context.Background()

	const maxRounds = 200
	for round := 1; ; round++ {
		if round > maxRounds {
			t.Fatalf("driveSaga(%s): still appending after %d rounds; the saga is not converging", transactionID, maxRounds)
		}
		before := totalEvents(t, store, transactionID)

		if err := ts.Resume(ctx, transactionID); err != nil {
			t.Fatalf("driveSaga(%s): resume transaction: %v", transactionID, err)
		}
		if xfers != nil {
			for _, childID := range knownChildren(t, store, transactionID) {
				if err := xfers.Resume(ctx, childID); err != nil {
					t.Fatalf("driveSaga(%s): resume child %s: %v", transactionID, childID, err)
				}
			}
			if err := ts.Resume(ctx, transactionID); err != nil {
				t.Fatalf("driveSaga(%s): resume transaction: %v", transactionID, err)
			}
		}

		if totalEvents(t, store, transactionID) == before {
			return round
		}
	}
}

// knownChildren lists every Transfer this Transaction has recorded anything
// about and that has a stream of its own, including the Reversals its rollback
// created — those are Transfer aggregates too, and they need resuming exactly
// like a forward child. A child whose intent was recorded but whose request was
// never made has no stream and is left out: the orchestrator only ever gets a
// trigger for an aggregate that has actually written something.
func knownChildren(t *testing.T, store eventstore.Store, transactionID string) []string {
	t.Helper()
	events, err := store.Load(context.Background(), AggregateType, transactionID)
	if err != nil {
		t.Fatalf("Load(%s): %v", transactionID, err)
	}

	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, e := range events {
		msg, err := e.Decode()
		if err != nil {
			t.Fatalf("Decode(%s): %v", e.EventType, err)
		}
		add(childEventTransferID(msg))
		// A Reversal is named only by the event that requested it.
		if requested, ok := msg.(*pb.TransferReversalRequestedWithinTransaction); ok {
			add(requested.GetReversalId())
		}
		if rolled, ok := msg.(*pb.TransferRolledBackWithinTransaction); ok {
			add(rolled.GetDetailId())
		}
	}

	existing := ids[:0]
	for _, id := range ids {
		child, err := store.Load(context.Background(), transfer.AggregateType, id)
		if err != nil {
			t.Fatalf("Load(transfer %s): %v", id, err)
		}
		if len(child) > 0 {
			existing = append(existing, id)
		}
	}
	return existing
}

// totalEvents counts the Transaction's own stream plus every child's, so a
// round that only moved a child still registers as progress.
func totalEvents(t *testing.T, store eventstore.Store, transactionID string) int {
	t.Helper()
	ctx := context.Background()
	events, err := store.Load(ctx, AggregateType, transactionID)
	if err != nil {
		t.Fatalf("Load(%s): %v", transactionID, err)
	}
	total := len(events)
	for _, childID := range knownChildren(t, store, transactionID) {
		child, err := store.Load(ctx, transfer.AggregateType, childID)
		if err != nil {
			t.Fatalf("Load(transfer %s): %v", childID, err)
		}
		total += len(child)
	}
	return total
}
