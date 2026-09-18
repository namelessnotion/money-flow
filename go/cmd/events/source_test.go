package main

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	txpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

// testSource connects to the test database, skipping when it is unreachable
// so `go test ./...` still works without Docker. Appends are permanent, so it
// refuses any database not named *_test.
func testSource(t *testing.T) (pgSource, *eventstore.PostgresStore) {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://money_flow:money_flow@localhost:5432/money_flow_test?sslmode=disable"
	}
	if parsed, err := url.Parse(dbURL); err != nil || !strings.HasSuffix(parsed.Path, "_test") {
		t.Fatalf("refusing to append to %q: the database name must end in _test", dbURL)
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err == nil {
		err = pool.Ping(context.Background())
	}
	if err != nil {
		t.Skipf("test database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pgSource{pool: pool}, eventstore.NewPostgresStore(pool)
}

func TestPgSourceReadsAfterTheCursorAndItsHoles(t *testing.T) {
	ctx := context.Background()
	src, store := testSource(t)
	id := uuid.NewV7().String()

	events := []proto.Message{
		&txpb.TransactionInitialized{Id: id}, &txpb.TransactionStarted{Id: id}, &txpb.TransactionCompleted{Id: id},
	}
	if err := store.Append(ctx, "transaction", id, 0, events...); err != nil {
		t.Fatal(err)
	}
	head, err := src.head(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The first of the three is asked for as a hole and the cursor sits on the
	// second, so the read is the hole plus what lies after — not the second.
	got, err := src.read(ctx, head-1, []int64{head - 2}, 10)
	if err != nil {
		t.Fatal(err)
	}

	var types []string
	for _, e := range got {
		if e.AggregateID != id {
			t.Fatalf("read another aggregate's event %+v", e)
		}
		types = append(types, e.EventType)
	}
	want := []string{"transaction.v1.TransactionInitialized", "transaction.v1.TransactionCompleted"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("read %v, want %v", types, want)
	}
}
