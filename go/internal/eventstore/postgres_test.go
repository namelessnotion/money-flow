package eventstore_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

// Shared with ruby/'s suite — the two migrators track themselves in separate
// tables (schema_migrations and ruby_schema_migrations), exactly as they
// already coexist in development.
const defaultTestDatabaseURL = "postgres://money_flow:money_flow@localhost:5432/money_flow_test?sslmode=disable"

// requireTestDatabase rejects a connection URL that doesn't name a test
// database. These tests append to the real event log, and the events table's
// append-only triggers make everything they write permanent — there is no
// cleaning up afterwards, so pointing them at a development database silently
// corrupts it. Fail rather than skip: a skip would let a misconfigured CI go
// green while testing nothing.
func requireTestDatabase(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("cannot parse database URL %q: %w", raw, err)
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	if !strings.HasSuffix(name, "_test") {
		return fmt.Errorf(
			"refusing to run against database %q: these tests write permanently to the append-only "+
				"event log, so the database name must end in _test. Set TEST_DATABASE_URL.", name)
	}
	return nil
}

// testPool connects to the events database, skipping the test when it isn't
// reachable so `go test ./...` still works without Docker running.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	// TEST_DATABASE_URL first: DATABASE_URL points at development wherever the
	// app itself runs, and these tests must never land there.
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		dbURL = defaultTestDatabaseURL
	}
	if err := requireTestDatabase(dbURL); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping: cannot configure pool for %s: %v", dbURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: no database at %s (run `docker compose up -d postgres`): %v", dbURL, err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func TestPostgresStore(t *testing.T) {
	pool := testPool(t)

	runStoreContract(t, func(t *testing.T) eventstore.Store {
		t.Helper()
		return eventstore.NewPostgresStore(pool)
	})
}

// The event log is immutable by design and the migration enforces that with
// triggers, not just convention. If someone later drops them, this fails.
func TestPostgresStoreIsAppendOnly(t *testing.T) {
	pool := testPool(t)
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
	pool := testPool(t)
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
	pool := testPool(t)
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
	pool := testPool(t)
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
