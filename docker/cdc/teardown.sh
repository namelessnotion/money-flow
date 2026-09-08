#!/bin/sh
# Reverses docker/cdc/setup.sh. Run from the repository root.
#
# The replication slot is the one thing `docker compose down` does NOT clean
# up: it lives in the long-lived postgres volume, and a slot left behind with
# no consumer pins WAL until the disk fills. Dropping the connector first is
# what makes the slot inactive and therefore droppable.
#
# The topics are deliberately left alone. Kafka has no volume, so `docker
# compose down` takes them with it; within one session, re-running setup.sh
# republishes into the topics that are already there.
set -eu

. "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/common.sh"

curl -sf -X DELETE "$CDC_CONNECT_URL/connectors/$CDC_CONNECTOR" >/dev/null 2>&1 &&
  echo "cdc: deleted connector $CDC_CONNECTOR" ||
  echo "cdc: connector $CDC_CONNECTOR already gone"

# A slot only becomes droppable once its walsender has exited.
i=0
while [ "$(docker compose exec -T postgres psql -U money_flow -d postgres -tAc \
      "SELECT active FROM pg_replication_slots WHERE slot_name = '$CDC_SLOT'")" = "t" ]; do
  i=$((i + 1))
  [ "$i" -gt 15 ] && echo "cdc: slot $CDC_SLOT still active, giving up" >&2 && exit 1
  sleep 1
done

docker compose exec -T postgres psql -U money_flow -d postgres -tAc \
  "SELECT pg_drop_replication_slot('$CDC_SLOT') FROM pg_replication_slots WHERE slot_name = '$CDC_SLOT'" >/dev/null
echo "cdc: dropped replication slot $CDC_SLOT (if it existed)"

docker compose exec -T postgres psql -U money_flow -d postgres -c "DROP DATABASE IF EXISTS \"$CDC_DATABASE\"" >/dev/null
echo "cdc: dropped database $CDC_DATABASE"
