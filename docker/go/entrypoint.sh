#!/bin/sh
# Waits for Postgres, applies pending event-store migrations, then execs the
# given command (default: go run ./cmd/server) — mirrors bin/db_migrate.sh.
set -eu

host="${DB_HOST:-postgres}"
port="${DB_PORT:-5432}"
user="${DB_USER:-money_flow}"
# The wait loop only needs a database that exists — without one,
# pg_isready defaults to the user name and each probe logs a FATAL in
# postgres. "postgres" is the maintenance database, present from initdb
# onward, so this holds even on a first boot of an empty volume.
db="${DB_NAME:-postgres}"

until pg_isready -h "$host" -p "$port" -U "$user" -d "$db" >/dev/null 2>&1; do
  echo "entrypoint: waiting for postgres at $host:$port..."
  sleep 1
done

go run ./cmd/migrate up

# The test database is shared with ruby/, whose entrypoint provisions it too:
# both containers start in parallel, so each creates it if missing rather than
# depending on the other's ordering. Both paths are idempotent, and the two
# migrators track themselves in separate tables. Skipped entirely where no test
# database is declared, so a real deployment never provisions one.
if [ -n "${TEST_DATABASE_URL:-}" ]; then
  base="${TEST_DATABASE_URL%%\?*}"
  query="${TEST_DATABASE_URL#"$base"}"
  test_db="${base##*/}"
  maintenance="${base%/*}/postgres${query}"

  if ! psql "$maintenance" -tAc "SELECT 1 FROM pg_database WHERE datname = '$test_db'" | grep -q 1; then
    # || true: ruby's entrypoint may win the race between this check and here.
    psql "$maintenance" -c "CREATE DATABASE \"$test_db\"" || true
  fi

  DATABASE_URL="$TEST_DATABASE_URL" go run ./cmd/migrate up
fi

exec "$@"
