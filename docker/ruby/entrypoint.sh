#!/bin/sh
# Waits for Postgres, applies pending Sequel migrations, then execs the given
# command (default: bin/server) — mirrors what a developer would run by hand.
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

bin/migrate up

# Only in environments that declare a test database (compose does; a real
# deployment would not), so `rspec` works the moment the container is up
# without the suite ever pointing at development data.
if [ -n "${TEST_DATABASE_URL:-}" ]; then
  bin/setup_test_db
fi

exec "$@"
