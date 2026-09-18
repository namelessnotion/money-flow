#!/bin/sh
# Publishes the development database's event log to Kafka: registers the
# Debezium connector (events-connector.json) that routes money_flow_dev's
# events table onto <aggregate_type>-events topics. See docs/adr/0001 and
# docs/cdc-tracer-bullet.md for the pipeline, docs/ach-transactions.md and
# docs/saga-orchestrator.md for what reads it.
#
# It starts from the current end of the log (snapshot.mode no_data): events
# written before it ran are never published.
#
# Reversed by docker/cdc/teardown.sh. Run from the repository root, with
# `docker compose up -d` already done.
set -eu

. "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/common.sh"

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
