package transaction

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

// seedInitialized writes spec's TransactionInitialized straight onto the
// stream, skipping StartInitializingTransaction entirely. The tests below are
// about what one saga run does, not about what an RPC does on the way in, and
// seeding keeps them saying so whichever handlers happen to drive.
func seedInitialized(t *testing.T, store eventstore.Store, txnID string, transfers map[string]*pb.Transfer) {
	t.Helper()
	err := store.Append(context.Background(), AggregateType, txnID, 0, &pb.TransactionInitialized{
		Id: txnID, FactoryName: "slicing_test", FactoryVersion: "1", Transfers: transfers,
	})
	if err != nil {
		t.Fatalf("seed TransactionInitialized: %v", err)
	}
}

// slicedChildren is how wide the fixtures below are: comfortably more than
// one slice, and a fixed number rather than a multiple of maxDispatchPerStep.
// Deriving it would mean raising that constant silently built an enormous
// test instead of failing a small one.
const slicedChildren = 20

// requireSliceable fails loudly if the constant has outgrown the fixture,
// rather than letting these tests quietly stop exercising more than one slice.
func requireSliceable(t *testing.T) {
	t.Helper()
	if maxDispatchPerStep >= slicedChildren {
		t.Fatalf("maxDispatchPerStep = %d is no longer below this fixture's %d children; "+
			"raise slicedChildren so these tests still cross a slice boundary",
			maxDispatchPerStep, slicedChildren)
	}
}

// independentRoots builds n independent roots. The dispatch tests below
// request them through an acceptingTransferClient, and the rollback tests
// never request them at all, so no ledger is needed to count slices.
func independentRoots(n int) map[string]*pb.Transfer {
	out := make(map[string]*pb.Transfer, n)
	for i := 0; i < n; i++ {
		id := testutil.ID(fmt.Sprintf("root-%d", i))
		out[id] = &pb.Transfer{
			Id: id, Amount: usd(100),
			FromWalletId: testutil.ID("w1"), ToWalletId: testutil.ID("w2"),
		}
	}
	return out
}

// requestedAndFailed seeds childID as a child that was requested and failed
// on its own. It moved no money, so rolling it back is one append to
// ABANDONED that reaches into no transferClient.
func requestedAndFailed(txnID, childID string) []proto.Message {
	return []proto.Message{
		&pb.TransferRequestedWithinTransaction{Id: txnID, TransferId: childID},
		&pb.TransferFailedWithinTransaction{Id: txnID, TransferId: childID, Reason: "seeded as failed"},
	}
}

func childrenOf(t *testing.T, store eventstore.Store, txnID string) map[string]childState {
	t.Helper()
	events, err := store.Load(context.Background(), AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	return children
}

func streamLen(t *testing.T, store eventstore.Store, txnID string) int {
	t.Helper()
	events, err := store.Load(context.Background(), AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return len(events)
}

// One run dispatches a slice and stops. Without the slice this fails by
// touching all 24 children on the first resume.
func TestDispatchReady_StopsAtTheSliceBoundary(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	client := newAcceptingTransferClient()
	server := NewServer(store, client)

	requireSliceable(t)
	txnID := testutil.ID("txn-slice")
	seedInitialized(t, store, txnID, independentRoots(slicedChildren))

	if err := server.Resume(context.Background(), txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if got := len(childrenOf(t, store, txnID)); got != maxDispatchPerStep {
		t.Fatalf("%d children touched after one resume, want exactly %d", got, maxDispatchPerStep)
	}
	if got := len(client.requested); got != maxDispatchPerStep {
		t.Fatalf("%d children requested after one resume, want exactly %d", got, maxDispatchPerStep)
	}
}

// The progress argument, as a test: no cursor is stored, so a slice that
// leaves work behind is only ever fetched again because it wrote something
// that becomes a trigger. A run that reported more work having appended
// nothing would strand the Transaction with nothing to wake it.
func TestDispatchReady_EverySliceAppendsAtLeastOneEvent(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, newAcceptingTransferClient())
	ctx := context.Background()

	requireSliceable(t)
	txnID := testutil.ID("txn-progress")
	total := slicedChildren
	seedInitialized(t, store, txnID, independentRoots(total))

	for round := 1; len(childrenOf(t, store, txnID)) < total; round++ {
		before := streamLen(t, store, txnID)
		if err := server.Resume(ctx, txnID); err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if after := streamLen(t, store, txnID); after <= before {
			t.Fatalf("resume %d left work outstanding but appended nothing (stream stayed at %d events): "+
				"nothing would ever trigger the next slice", round, after)
		}
		if round > total {
			t.Fatalf("resumed %d times without touching all %d children", round, total)
		}
	}
}

// mintSourceRoots builds n independent roots that each mint their own source
// Token, so they need no pre-funded balance and contend for nothing.
func mintSourceRoots(n int, from, to string) map[string]*pb.Transfer {
	out := make(map[string]*pb.Transfer, n)
	for i := 0; i < n; i++ {
		id := testutil.ID(fmt.Sprintf("mint-%d", i))
		out[id] = &pb.Transfer{
			Id: id, Amount: usd(100), FromWalletId: from, ToWalletId: to,
			MintSource: true,
		}
	}
	return out
}

// A sliced Transaction still finishes — it just takes several rounds of the
// trigger loop. Asserted from both sides: finishing in one round would mean
// the slice did nothing, and never finishing would mean a slice never resumed.
func TestDispatchReady_SlicedTransactionCompletesAcrossResumes(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	lc := ledger.NewFakeClient()
	bankAccount, cash, _, _ := achWallets(t, store)

	xferServer := newTransferServer(store, lc)
	server := NewServer(store, xferServer)
	ctx := context.Background()

	requireSliceable(t)
	txnID := testutil.ID("txn-complete")
	total := slicedChildren
	seedInitialized(t, store, txnID, mintSourceRoots(total, bankAccount, cash))

	rounds := driveSaga(t, server, xferServer, store, txnID)

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateCompleted {
		t.Fatalf("state = %v, want completed", got)
	}

	wantAtLeast := (total + maxDispatchPerStep - 1) / maxDispatchPerStep
	if rounds < wantAtLeast {
		t.Errorf("completed in %d rounds, want at least %d — a slice of %d cannot place %d children in fewer",
			rounds, wantAtLeast, maxDispatchPerStep, total)
	}
	children := childrenOf(t, store, txnID)
	for id, st := range children {
		if st != childCompleted {
			t.Errorf("child %s = %v, want completed", id, st)
		}
	}
	if len(children) != total {
		t.Errorf("%d children on the stream, want all %d", len(children), total)
	}
}

// appendAll writes events onto txnID's stream in order, continuing from
// wherever it currently ends.
func appendAll(t *testing.T, store eventstore.Store, txnID string, events ...proto.Message) {
	t.Helper()
	ctx := context.Background()
	for _, event := range events {
		existing, err := store.Load(ctx, AggregateType, txnID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if err := store.Append(ctx, AggregateType, txnID, int64(len(existing)), event); err != nil {
			t.Fatalf("Append(%T) error = %v", event, err)
		}
	}
}

// A rollback must never declare itself failed while it still holds a child it
// could reverse. That is what the filter-before-slice in rollbackNext
// protects: an unfiltered slice can be made up entirely of already-stuck
// children, make no progress, and fall through to blocked — recording
// TransactionRollbackFailed with reversible children left unreversed. Money
// left out rather than put back.
//
// Which children a slice draws is random (readyToRollback ranges a map), so a
// single unfiltered run might happen to include the reversible one. The
// assertion is repeated over fresh state to make that escape vanishingly
// unlikely, while staying deterministic against correct code, where the
// reversible child is always in the slice because the stuck ones are never
// candidates.
func TestRollbackNext_NeverReportsFailedWhileAChildCouldStillBeReversed(t *testing.T) {
	t.Parallel()
	requireSliceable(t)

	for attempt := 0; attempt < 12; attempt++ {
		store := eventstore.NewMemoryStore()
		server := NewServer(store, nil)
		ctx := context.Background()

		txnID := testutil.ID(fmt.Sprintf("txn-mixed-%d", attempt))
		specs := independentRoots(slicedChildren)
		seedInitialized(t, store, txnID, specs)

		// Every child failed on its own (so rolling one back is an append to
		// ABANDONED and needs no transferClient), and all but one is already
		// stuck.
		var reversible string
		seeded := []proto.Message{&pb.TransactionStarted{Id: txnID}}
		for childID := range specs {
			seeded = append(seeded, requestedAndFailed(txnID, childID)...)
		}
		seeded = append(seeded, &pb.TransactionRollbackStarted{Id: txnID, Reason: "seeded"})
		for childID := range specs {
			if reversible == "" {
				reversible = childID
				continue
			}
			seeded = append(seeded, &pb.TransferRollbackFailedWithinTransaction{
				Id: txnID, TransferId: childID, Reason: "seeded as stuck",
			})
		}
		appendAll(t, store, txnID, seeded...)

		if err := server.Resume(ctx, txnID); err != nil {
			t.Fatalf("Resume() error = %v", err)
		}

		events, err := store.Load(ctx, AggregateType, txnID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		children, err := foldChildStates(events)
		if err != nil {
			t.Fatalf("foldChildStates() error = %v", err)
		}
		// Reaching rollback_failed is correct here — the other children really
		// are stuck. What must not happen is reaching it with this one still
		// unreversed, and the final stream saying it was rolled back is
		// exactly that guarantee.
		if children[reversible] != childRolledBack {
			t.Fatalf("attempt %d: reversible child %s = %v, want rolled back — the slice passed it over and "+
				"the rollback gave up with money still out", attempt, reversible, children[reversible])
		}
	}
}

// Once nothing rollbackable is left, the Transaction does say so rather than
// sitting in rollback_started with no explanation.
func TestRollbackNext_AllStuckChildrenReachRollbackFailed(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	requireSliceable(t)
	txnID := testutil.ID("txn-stuck")
	specs := independentRoots(slicedChildren)
	seedInitialized(t, store, txnID, specs)

	seeded := []proto.Message{&pb.TransactionStarted{Id: txnID}}
	for childID := range specs {
		seeded = append(seeded,
			&pb.TransferRequestedWithinTransaction{Id: txnID, TransferId: childID},
			&pb.TransferCompletedWithinTransaction{Id: txnID, TransferId: childID},
		)
	}
	seeded = append(seeded, &pb.TransactionRollbackStarted{Id: txnID, Reason: "seeded"})
	for childID := range specs {
		seeded = append(seeded, &pb.TransferRollbackFailedWithinTransaction{
			Id: txnID, TransferId: childID, Reason: "seeded as stuck",
		})
	}
	appendAll(t, store, txnID, seeded...)

	if err := server.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := topLevelState(events); got != stateRollbackFailed {
		t.Fatalf("state = %v, want rollback_failed", got)
	}
}

// Rollback slices for the same reason forward dispatch does: each child's
// reversal is a whole Transfer running its own saga, and under the settlement
// boundary a cancel does its TigerBeetle void inline.
func TestRollbackNext_StopsAtTheSliceBoundary(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	requireSliceable(t)
	txnID := testutil.ID("txn-rollback-slice")
	specs := independentRoots(slicedChildren)
	seedInitialized(t, store, txnID, specs)

	// Failed children moved no money, so rolling one back is an append to
	// ABANDONED and reaches into no transferClient — which is what lets this
	// count slices without a ledger.
	seeded := []proto.Message{
		&pb.TransactionStarted{Id: txnID},
	}
	for childID := range specs {
		seeded = append(seeded, requestedAndFailed(txnID, childID)...)
	}
	seeded = append(seeded, &pb.TransactionRollbackStarted{Id: txnID, Reason: "seeded"})
	appendAll(t, store, txnID, seeded...)

	if err := server.Resume(ctx, txnID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	events, err := store.Load(ctx, AggregateType, txnID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	children, err := foldChildStates(events)
	if err != nil {
		t.Fatalf("foldChildStates() error = %v", err)
	}
	rolledBack := 0
	for _, st := range children {
		if st == childRolledBack {
			rolledBack++
		}
	}
	if rolledBack != maxDispatchPerStep {
		t.Fatalf("%d children rolled back after one resume, want exactly %d", rolledBack, maxDispatchPerStep)
	}
	if topLevelState(events) != stateRollbackStarted {
		t.Errorf("state = %v, want rollback_started (the rollback is sliced, not finished)", topLevelState(events))
	}
}

// And it still finishes, across as many runs as the slice needs.
func TestRollbackNext_SlicedRollbackCompletesAcrossResumes(t *testing.T) {
	t.Parallel()
	store := eventstore.NewMemoryStore()
	server := NewServer(store, nil)
	ctx := context.Background()

	requireSliceable(t)
	txnID := testutil.ID("txn-rollback-complete")
	specs := independentRoots(slicedChildren)
	seedInitialized(t, store, txnID, specs)

	seeded := []proto.Message{&pb.TransactionStarted{Id: txnID}}
	for childID := range specs {
		seeded = append(seeded, requestedAndFailed(txnID, childID)...)
	}
	seeded = append(seeded, &pb.TransactionRollbackStarted{Id: txnID, Reason: "seeded"})
	appendAll(t, store, txnID, seeded...)

	rounds := 0
	for {
		rounds++
		if rounds > slicedChildren {
			t.Fatalf("rollback still unfinished after %d resumes", rounds)
		}
		if err := server.Resume(ctx, txnID); err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		events, err := store.Load(ctx, AggregateType, txnID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if topLevelState(events) == stateRolledBack {
			break
		}
	}

	wantAtLeast := (slicedChildren + maxDispatchPerStep - 1) / maxDispatchPerStep
	if rounds < wantAtLeast {
		t.Errorf("rolled back in %d resumes, want at least %d", rounds, wantAtLeast)
	}
}
