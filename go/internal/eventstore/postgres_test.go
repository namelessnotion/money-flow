package eventstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

func TestPostgresStore(t *testing.T) {
	pool := testutil.Pool(t)

	runStoreContract(t, func(t *testing.T) eventstore.Store {
		t.Helper()
		return eventstore.NewPostgresStore(pool)
	})
}

// The event log is immutable by design and the migration enforces that with
// triggers, not just convention. If someone later drops them, this fails.
func TestPostgresStoreIsAppendOnly(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	store := eventstore.NewPostgresStore(pool)
	id := uniqueID(t)
	if err := store.Append(ctx, "holder", id, 0, &pb.HolderEstablished{Id: id}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	t.Run("update is rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`UPDATE events SET event_type = 'tampered' WHERE aggregate_type = 'holder' AND aggregate_id = $1`, id)
		if err == nil {
			t.Fatal("UPDATE on events succeeded, want it blocked by the append-only trigger")
		}
	})

	t.Run("delete is rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`DELETE FROM events WHERE aggregate_type = 'holder' AND aggregate_id = $1`, id)
		if err == nil {
			t.Fatal("DELETE on events succeeded, want it blocked by the append-only trigger")
		}
	})
}

// The whole point of expectedSeq is that exactly one of two racing writers
// wins. This drives that race through the real UNIQUE constraint rather than
// trusting the single-threaded path.
func TestPostgresStoreConcurrentAppendHasOneWinner(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	store := eventstore.NewPostgresStore(pool)
	id := uniqueID(t)

	const writers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
	)

	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			err := store.Append(ctx, "holder", id, 0, &pb.HolderEstablished{Id: id})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, eventstore.ErrConcurrencyConflict):
				conflicts++
			default:
				t.Errorf("Append() unexpected error = %v", err)
			}
		}()
	}
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d writers succeeded, want exactly 1", succeeded)
	}
	if conflicts != writers-1 {
		t.Errorf("%d writers conflicted, want %d", conflicts, writers-1)
	}

	events, err := store.Load(ctx, "holder", id)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Load() returned %d events, want exactly 1", len(events))
	}
}

// The multi-stream equivalent of the single-stream race above: several writers
// racing the identical provisioning batch must produce exactly one winner, with
// every stream in the batch holding only that winner's events.
func TestPostgresStoreConcurrentAtomicAppendHasOneWinner(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	store := eventstore.NewPostgresStore(pool)
	holderID := uniqueID(t)
	walletIDs := []string{uniqueID(t), uniqueID(t), uniqueID(t)}

	batch := func() []eventstore.StreamWrite {
		writes := []eventstore.StreamWrite{{
			AggregateType: "holder", AggregateID: holderID, ExpectedSeq: 0,
			Events: []proto.Message{&pb.HolderEstablished{Id: holderID}},
		}}
		for _, w := range walletIDs {
			writes = append(writes, eventstore.StreamWrite{
				AggregateType: "wallet", AggregateID: w, ExpectedSeq: 0,
				Events: []proto.Message{&pb.HolderAddedWallet{Id: holderID, WalletId: w}},
			})
		}
		return writes
	}

	const writers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
	)

	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			err := store.AppendAtomic(ctx, batch()...)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, eventstore.ErrConcurrencyConflict):
				conflicts++
			default:
				t.Errorf("AppendAtomic() unexpected error = %v", err)
			}
		}()
	}
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d writers succeeded, want exactly 1", succeeded)
	}
	if conflicts != writers-1 {
		t.Errorf("%d writers conflicted, want %d", conflicts, writers-1)
	}

	// Every stream in the batch holds exactly one event — the losers left nothing.
	for _, tc := range append([]string{holderID}, walletIDs...) {
		aggType := "wallet"
		if tc == holderID {
			aggType = "holder"
		}
		events, err := store.Load(ctx, aggType, tc)
		if err != nil {
			t.Fatalf("Load(%s/%s) error = %v", aggType, tc, err)
		}
		if len(events) != 1 {
			t.Errorf("Load(%s/%s) returned %d events, want exactly 1", aggType, tc, len(events))
		}
	}
}

// global_seq orders events across every aggregate, which is what projections
// and orchestrators read forward from.
func TestPostgresStoreGlobalSeqIsMonotonic(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	store := eventstore.NewPostgresStore(pool)
	id := uniqueID(t)

	if err := store.Append(ctx, "holder", id, 0,
		&pb.HolderEstablished{Id: id},
		&pb.HolderAddedWallet{Id: id, WalletId: "w1"},
	); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	events, err := store.Load(ctx, "holder", id)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("Load() returned %d events, want 2", len(events))
	}
	if events[0].GlobalSeq >= events[1].GlobalSeq {
		t.Errorf("global_seq not increasing: %d then %d", events[0].GlobalSeq, events[1].GlobalSeq)
	}
}
