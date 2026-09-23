package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Shared with ruby/'s suite — the two migrators track themselves in separate
// tables (schema_migrations and ruby_schema_migrations), exactly as they
// already coexist in development.
const defaultTestDatabaseURL = "postgres://money_flow:money_flow@localhost:5432/money_flow_test?sslmode=disable"

// requireTestDatabase rejects a connection URL that doesn't name a test
// database. Tests on the pool append to the real event log, and the events
// table's append-only triggers make everything they write permanent — there
// is no cleaning up afterwards, so pointing them at a development database
// silently corrupts it. Fail rather than skip: a skip would let a
// misconfigured CI go green while testing nothing.
func requireTestDatabase(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("cannot parse database URL %q: %w", raw, err)
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	if !strings.HasSuffix(name, "_test") {
		return fmt.Errorf(
			"refusing to run against database %q: these tests write permanently to the append-only "+
				"event log, so the database name must end in _test (set TEST_DATABASE_URL)", name)
	}
	return nil
}

// Pool connects to the events test database, skipping the test when it isn't
// reachable so `go test ./...` still works without Docker running. The log is
// shared and append-only, so a test using it must mint its own ids and assert
// only about the rows it wrote.
func Pool(t *testing.T) *pgxpool.Pool {
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
