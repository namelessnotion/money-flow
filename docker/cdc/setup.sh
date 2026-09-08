#!/bin/sh
# Brings up the CDC tracer bullet: a scratch database, the events schema in it,
# and the Debezium connector that publishes it to Kafka. See
# docs/cdc-tracer-bullet.md.
#
# Everything this creates is torn down by docker/cdc/teardown.sh. Run from the
# repository root, with `docker compose up -d` already done.
set -eu

. "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/common.sh"

# The scratch database exists so the tracer bullet's events — which are
# permanent, the table being append-only — never land in money_flow_dev.
if [ "$(docker compose exec -T postgres psql -U money_flow -d postgres -tAc \
      "SELECT 1 FROM pg_database WHERE datname = '$CDC_DATABASE'")" != "1" ]; then
  docker compose exec -T postgres psql -U money_flow -d postgres -c "CREATE DATABASE \"$CDC_DATABASE\""
  echo "cdc: created database $CDC_DATABASE"
fi

# The same migrations the real event store runs, so the pipeline sees the real
# table definition — triggers, identity column and all.
docker compose exec -T \
  -e DATABASE_URL="postgres://money_flow:money_flow@postgres:5432/$CDC_DATABASE?sslmode=disable" \
  go go run ./cmd/migrate up

printf 'cdc: waiting for Kafka Connect at %s' "$CDC_CONNECT_URL"
until curl -sf "$CDC_CONNECT_URL/connectors" >/dev/null 2>&1; do
  printf '.'
  sleep 2
done
echo

# PUT the config rather than POST a new connector, so re-running this updates
# it in place instead of failing on a name that already exists.
curl -sf -X PUT -H 'Content-Type: application/json' \
  --data-binary "@$CDC_CONNECTOR_FILE" \
  "$CDC_CONNECT_URL/connectors/$CDC_CONNECTOR/config" >/dev/null

echo "cdc: registered connector $CDC_CONNECTOR"
sleep 3
curl -s "$CDC_CONNECT_URL/connectors/$CDC_CONNECTOR/status"
echo
