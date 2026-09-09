package saga

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	walletpb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/token"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

func usd(minorUnits uint64) *sharedpb.Money {
	return &sharedpb.Money{MinorUnits: minorUnits, Currency: "USD"}
}

// world is one wired-up system under test: the real Transfer and Transaction
// servers over a shared store, plus the orchestrator driving them. Nothing
// here is a fake except the ledger and the transport.
type world struct {
	t            *testing.T
	store        *countingStore
	ledger       ledger.Client
	transfers    *transfer.Server
	transactions *transaction.Server
	orchestrator *Orchestrator
}

func newWorld(t *testing.T, base eventstore.Store, lc ledger.Client) *world {
	t.Helper()
	store := &countingStore{Store: base}

	// Wired exactly as the commands wire it, so these tests exercise the real
	// cross-aggregate mechanisms rather than a convenient arrangement of them.
	servers := Wire(store, lc)

	return &world{
		t: t, store: store, ledger: lc,
		transfers: servers.Transfer, transactions: servers.Transaction,
		orchestrator: servers.Orchestrator(),
	}
}

// deliver hands the orchestrator the trigger a published message would have
// produced for this aggregate. The envelope is built the way the connector
// builds it, payload included, so what the orchestrator ignores is genuinely
// present rather than absent from the fixture.
func (w *world) deliver(aggregateType, aggregateID string) error {
	w.t.Helper()
	value := `{"payload":"c29tZSBvcGFxdWUgcHJvdG9idWY=","aggregate_type":"` + aggregateType +
		`","event_type":"` + aggregateType + `.v1.SomethingHappened","sequence":1,"global_seq":1}`
	trigger, err := ParseTrigger([]byte(aggregateID), []byte(value))
	if err != nil {
		w.t.Fatalf("ParseTrigger() error = %v", err)
	}
	return w.orchestrator.Handle(context.Background(), trigger)
}

// mustDeliver is deliver for the steps a test is not itself examining.
func (w *world) mustDeliver(aggregateType, aggregateID string) {
	w.t.Helper()
	if err := w.deliver(aggregateType, aggregateID); err != nil {
		w.t.Fatalf("Handle(%s %s) error = %v", aggregateType, aggregateID, err)
	}
}

// transactionState folds the Transaction's own stream, rather than asking the
// ResumeTransaction RPC. The RPC runs the saga before it answers, so using it
// here would drive the very progress these tests are asserting has not
// happened yet.
func (w *world) transactionState(transactionID string) transactionpb.TransactionState {
	w.t.Helper()
	events, err := w.store.Load(context.Background(), transaction.AggregateType, transactionID)
	if err != nil {
		w.t.Fatalf("Load() error = %v", err)
	}

	state := transactionpb.TransactionState_TRANSACTION_STATE_UNSPECIFIED
	for _, e := range events {
		if s, ok := topLevelStates[e.EventType]; ok {
			state = s
		}
	}
	return state
}

// topLevelStates maps each top-level transition event to the state it leaves
// the Transaction in — the same last-one-wins fold transaction.topLevelState
// performs on its own unexported enum.
var topLevelStates = map[string]transactionpb.TransactionState{
	eventstore.EventType(&transactionpb.TransactionInitialized{}):     transactionpb.TransactionState_TRANSACTION_STATE_INITIALIZED,
	eventstore.EventType(&transactionpb.TransactionRejected{}):        transactionpb.TransactionState_TRANSACTION_STATE_REJECTED,
	eventstore.EventType(&transactionpb.TransactionStarted{}):         transactionpb.TransactionState_TRANSACTION_STATE_STARTED,
	eventstore.EventType(&transactionpb.TransactionRollbackStarted{}): transactionpb.TransactionState_TRANSACTION_STATE_ROLLBACK_STARTED,
	eventstore.EventType(&transactionpb.TransactionCompleted{}):       transactionpb.TransactionState_TRANSACTION_STATE_COMPLETED,
	eventstore.EventType(&transactionpb.TransactionRolledBack{}):      transactionpb.TransactionState_TRANSACTION_STATE_ROLLED_BACK,
	eventstore.EventType(&transactionpb.TransactionRollbackFailed{}):  transactionpb.TransactionState_TRANSACTION_STATE_ROLLBACK_FAILED,
}

func (w *world) outcome(transferID string) transfer.OutcomeKind {
	w.t.Helper()
	got, err := transfer.Outcome(context.Background(), w.store, transferID)
	if err != nil {
		w.t.Fatalf("Outcome() error = %v", err)
	}
	return got
}

// settle walks a staged Transfer through the two confirmations the outside
// world owns. Neither runs a saga on the owning Transaction, which is the
// whole point: nothing but a trigger can tell the Transaction its child moved.
func (w *world) settle(transferID string) {
	w.t.Helper()
	ctx := context.Background()
	if _, err := w.transfers.ConfirmStagedTransfer(ctx, &transferpb.ConfirmStagedTransferRequest{Id: transferID}); err != nil {
		w.t.Fatalf("ConfirmStagedTransfer(%s) error = %v", transferID, err)
	}
	if _, err := w.transfers.PostPendingTransfer(ctx, &transferpb.PostPendingTransferRequest{Id: transferID}); err != nil {
		w.t.Fatalf("PostPendingTransfer(%s) error = %v", transferID, err)
	}
}

// initialize records TransactionInitialized directly, which is what disables
// the synchronous dispatch: StartInitializingTransaction would append this
// same event and then immediately run the saga in process. Written this way,
// the Transaction exists and has done nothing, and only a trigger can move it.
func (w *world) initialize(transactionID string, transfers map[string]*transactionpb.Transfer, deps map[string]*transactionpb.TransferIdList) {
	w.t.Helper()
	err := w.store.Append(context.Background(), transaction.AggregateType, transactionID, 0, &transactionpb.TransactionInitialized{
		Id: transactionID, Transfers: transfers, TransferDependency: deps,
	})
	if err != nil {
		w.t.Fatalf("seed TransactionInitialized: %v", err)
	}
}

func (w *world) openWallet(walletID string, allows sharedpb.Allows) {
	w.t.Helper()
	_, err := wallet.NewServer(w.store).Open(context.Background(), &walletpb.OpenRequest{
		Id: walletID, HolderId: testutil.ID("h1"), Name: "test wallet", Allows: allows,
	})
	if err != nil {
		w.t.Fatalf("open wallet: %v", err)
	}
}

// mintAndFundToken gives a Wallet ordinary, FIFO-selectable balance, for the
// legs that do not mint their own source.
func (w *world) mintAndFundToken(walletID, tokenID string, capacity *sharedpb.Money) {
	w.t.Helper()
	ctx := context.Background()
	resp, err := token.NewServer(w.store, w.ledger).Mint(ctx, &tokenpb.MintRequest{Id: tokenID, WalletId: walletID, Capacity: capacity})
	if err != nil {
		w.t.Fatalf("mint token: %v", err)
	}
	if resp.GetTokenMintRejected() != nil {
		w.t.Fatalf("mint token rejected: %v", resp.GetTokenMintRejected())
	}

	source := ledger.Account{ID: testutil.ID("external-funding-" + tokenID), Currency: capacity.GetCurrency()}
	if _, err := w.ledger.CreateAccounts(ctx, []ledger.Account{source}); err != nil {
		w.t.Fatalf("fund token: create external account: %v", err)
	}
	results, err := w.ledger.CreateTransfers(ctx, []ledger.Transfer{{
		ID: testutil.ID("fund-" + tokenID), DebitAccountID: source.ID, CreditAccountID: tokenID,
		MinorUnits: capacity.GetMinorUnits(), Currency: capacity.GetCurrency(), Kind: ledger.TransferKindRegular,
	}})
	if err != nil {
		w.t.Fatalf("fund token: transfer: %v", err)
	}
	if results[0].Result != ledger.TransferResultOK {
		w.t.Fatalf("fund token: transfer result = %v, want OK", results[0].Result)
	}
}

// countingStore tallies every event that reaches the log, whatever stream it
// lands on. "Changed nothing" is only a meaningful claim if a stray write to
// an Operation or a Token would show up too, so the count is taken across the
// whole store rather than over a list of streams the test remembered to name.
//
// It also counts the optimistic-concurrency conflicts the store handed back,
// which is how the concurrency test proves it provoked a real race rather than
// merely running two goroutines.
type countingStore struct {
	eventstore.Store

	mu        sync.Mutex
	appended  int
	conflicts int

	// barrier, when set, holds writes to one named stream until enough of them
	// are pending at once to guarantee all but one is stale.
	barrier *appendBarrier
}

func (s *countingStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	s.hold(aggregateType, aggregateID)
	err := s.Store.Append(ctx, aggregateType, aggregateID, expectedSeq, events...)
	s.record(len(events), err)
	return err
}

func (s *countingStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	n := 0
	for _, w := range writes {
		s.hold(w.AggregateType, w.AggregateID)
		n += len(w.Events)
	}
	err := s.Store.AppendAtomic(ctx, writes...)
	s.record(n, err)
	return err
}

func (s *countingStore) hold(aggregateType, aggregateID string) {
	if s.barrier != nil {
		s.barrier.arrive(aggregateType, aggregateID)
	}
}

func (s *countingStore) record(events int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		s.appended += events
	case errors.Is(err, eventstore.ErrConcurrencyConflict):
		s.conflicts++
	}
}

func (s *countingStore) counts() (appended, conflicts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appended, s.conflicts
}

// appendBarrier makes a concurrency conflict certain instead of likely. Every
// "load, decide, append" loop in the sagas reads a stream's length and then
// appends at that sequence, so holding two such appends until both are pending
// guarantees the second one is writing against a sequence that has moved.
//
// It cannot be satisfied by one goroutine arriving twice, because the first
// arrival blocks: reaching the count takes two genuinely concurrent writers.
type appendBarrier struct {
	aggregateType string
	aggregateID   string

	mu       sync.Mutex
	waiting  int
	need     int
	released bool
	gate     chan struct{}
}

func newAppendBarrier(aggregateType, aggregateID string, need int) *appendBarrier {
	return &appendBarrier{aggregateType: aggregateType, aggregateID: aggregateID, need: need, gate: make(chan struct{})}
}

func (b *appendBarrier) arrive(aggregateType, aggregateID string) {
	if aggregateType != b.aggregateType || aggregateID != b.aggregateID {
		return
	}

	b.mu.Lock()
	if b.released {
		b.mu.Unlock()
		return
	}
	b.waiting++
	if b.waiting >= b.need {
		b.released = true
		close(b.gate)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	// Bounded, so a fixture that never produces enough concurrent writers
	// fails the test's own assertion rather than hanging the suite.
	select {
	case <-b.gate:
	case <-time.After(2 * time.Second):
	}
}

func (b *appendBarrier) tripped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.released
}
