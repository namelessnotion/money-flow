# Sourced by setup.sh and teardown.sh. Everything here is derived from
# events-connector.json so that file stays the single owner of the connector's
# identity: change the slot or database there and teardown still finds it.
CDC_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
CDC_CONNECTOR_FILE="$CDC_DIR/events-connector.json"

# The connector name is ours to choose — it is a Connect REST path segment, not
# something the config file carries.
CDC_CONNECTOR="money-flow-events"
CDC_CONNECT_URL="${CONNECT_URL:-http://localhost:8083}"

# Reads one string setting out of the connector config. Deliberately a sed
# one-liner rather than a JSON parser: this keeps the scripts to curl, sed and
# docker, which is everything else the repo's scripts already assume.
cdc_setting() {
  sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$CDC_CONNECTOR_FILE"
}

CDC_DATABASE="$(cdc_setting 'database\.dbname')"
CDC_SLOT="$(cdc_setting 'slot\.name')"

if [ -z "$CDC_DATABASE" ] || [ -z "$CDC_SLOT" ]; then
  echo "cdc: could not read database.dbname / slot.name from $CDC_CONNECTOR_FILE" >&2
  exit 1
fi
