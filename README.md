<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
  <img alt="MoneyFlow" src="assets/logo-light.svg" height="72">
</picture>

Event sourced tokenized transaction system

## Status

Early, but a money-moving path works end to end:

- Onboarding an `Entity` in Ruby provisions a `Holder` and its `Wallet`s in Go
  over Twirp, recorded as immutable events; the Vue client lists entities.
- An **ACH deposit or withdrawal** started from GraphQL runs in Go as a two-leg
  Transaction: a staged real leg that waits on the ACH network, and a shadow
  leg that follows it. Settlement and returns are reported through GraphQL,
  standing in for an ACH provider.
- Go's events are published to Kafka by Debezium (CDC). The Go orchestrator
  drives sagas forward from them, and a Ruby consumer folds them into a read
  model that GraphQL serves.

See [docs/ach-transactions.md](docs/ach-transactions.md) for the ACH flow.

## Structure

```
proto/    Protobuf domain messages and Twirp service definitions, shared by go/ and ruby/
go/       Event-sourced token based transaction backend (Go + Twirp), backed by TigerBeetle for accounting
ruby/     Business backend (Ruby + GraphQL), orchestrates money flow via the go/ backend
client/   Frontend (Vue 3 + Apollo Client 4 + TailwindCSS), talks to ruby/ over GraphQL
docker/   Dockerfiles and entrypoints for each service, wired together by docker-compose.yml
```

### `proto/`

Domain command/event messages and Twirp service definitions
(`holder`, `wallet`, `token`, `transaction`, `transfer`, `operation`,
`shared`), compiled to both Go and Ruby with `bin/generate_protos.sh`.

### `go/`

Event-sourced backend. Commands are validated and recorded as domain events
in an append-only event log (`internal/eventstore`, PostgreSQL-backed, one
table); the event log is the source of truth for intent. TigerBeetle handles
low-level token accounting. Exposed as Twirp RPC services (`internal/holder`,
`internal/wallet`, `internal/transfer`, `internal/transaction`, …) reachable
directly on `:8080` or via the proxy at `https://rpc.local.namelessnotion.com`.
`cmd/orchestrator` consumes the published events and drives the sagas forward
([docs/saga-orchestrator.md](docs/saga-orchestrator.md)).

### `ruby/`

Business backend. Exposes GraphQL (`app/graphql`) backed by Sequel models
(`app/models`) over PostgreSQL, and orchestrates money-flow operations
(`app/services`) by calling the `go/` backend over Twirp. Runs on Falcon,
reachable directly on `:9292` or via the proxy at
`https://graphql.local.namelessnotion.com`. `bin/consumer` reads the published
events into a lagging read model (`app/consumer`,
[ruby/docs/adr](ruby/docs/adr)). Vocabulary: [ruby/CONTEXT.md](ruby/CONTEXT.md).

### `client/`

Vue 3 + TypeScript SPA. Apollo Client (via `@vue/apollo-composable`) queries
the Ruby GraphQL API; TailwindCSS for styling. Served by Vite, reachable
directly on `:5173` or via the proxy at `https://app.local.namelessnotion.com`.

## Running locally

Everything runs via Docker Compose: PostgreSQL, TigerBeetle, the Go and Ruby
backends, the Vite dev server, Kafka and Kafka Connect, and an nginx reverse
proxy that fronts the apps under `*.local.namelessnotion.com`. The only
prerequisites are Docker with Compose v2, and free host ports `5432`, `3000`,
`5173`, `8080`, `8083` and `9292` (plus `80`/`443` for the proxy).

```bash
make up        # docker compose up -d
```

This starts:

| Service          | Direct port | Proxied at                                 |
| ---------------- | ----------- | ------------------------------------------ |
| `client` (Vite)  | `:5173`     | `https://app.local.namelessnotion.com`     |
| `ruby` (GraphQL) | `:9292`     | `https://graphql.local.namelessnotion.com` |
| `go` (Twirp/RPC) | `:8080`     | `https://rpc.local.namelessnotion.com`     |

The Go and Ruby services each run pending migrations on boot. To add the
proxy subdomains, point them at `127.0.0.1` in `/etc/hosts`, then generate a
browser-trusted local cert:

```bash
make ssl       # bin/setup_local_ssl.sh, requires mkcert; then restarts the proxy
```

Until `make ssl` has been run, the proxy serves a self-signed placeholder, so
HTTPS still works, just with a browser warning.

Other Makefile targets:

```bash
make down      # docker compose down (run `make cdc-down` first if CDC is up)
make restart   # docker compose restart
make migrate   # run both migrators out-of-band, without restarting go/ruby
```

### The event pipeline (CDC) — **required, not optional**

Nothing advances a saga except `orchestrator`, and it has nothing to consume
until the Debezium connector is registered
([go ADR 0006](go/docs/adr/0006-synchronous-dispatch-removed-from-the-rpc-surface.md)).
`make up` alone gets you an API that accepts work and never runs any of it:
every Transaction sits in `initialized`, no money moves, and nothing errors.

`docker compose up -d` does start the containers — the `cdc` compose profile
these docs used to describe was removed in 0a3818a — but registering the
connector is a separate step and always was:

```bash
make cdc-up            # register the Debezium connector: money_flow_dev -> Kafka
make orchestrator-up   # Go saga orchestrator — nothing runs without it
make consumer-up       # Ruby read-model consumer
make jobs-up           # Redis + Resque worker + resque-scheduler (ACH submission, clearing, disbursement)
```

| Piece             | What it does                                                                 | Logs                     |
| ----------------- | ---------------------------------------------------------------------------- | ------------------------ |
| connector         | Publishes `money_flow_dev`'s `events` table to `<aggregate>-events` topics   | —                        |
| `orchestrator`    | **Runs** every Transfer and Transaction saga, from `transfer-events` / `transaction-events` | `make orchestrator-logs` |
| `ruby-consumer`   | Projects the same topics into `transaction_projections` / `transfer_projections` | `make consumer-logs`     |
| `resque-scheduler` / `resque-worker` | Submits ready ACH entries every minute; clears deposits 3 Fed business days after they complete; disburses Repayments every 5 minutes | `make jobs-logs` |

Publication starts at the current end of the log; events written before
`make cdc-up` are never published. That matters more than it reads: an
aggregate whose events were never published has no wake-up coming and no RPC
can give it one. `make resume-open` drives everything still in flight, by hand
— run it after starting CDC late, and after any orchestrator repair.

Both consumers **halt** (exit non-zero, nothing committed) on a message they
cannot process, by design — check their logs if state stops moving.

Tear down in this order, **before** `make down`. The connector's replication
slot lives in the Postgres volume and pins WAL until it is dropped:

```bash
make jobs-down consumer-down orchestrator-down
make cdc-down
```

## Running an ACH Transaction end to end

With the stack and the event pipeline up (`make up`, then
`make cdc-up orchestrator-up consumer-up jobs-up` — **all four**, see above),
drive a deposit through GraphQL at
`http://localhost:9292/graphql`. The snippets need `curl` and
[`jq`](https://jqlang.org/). `gql` sends a query, and passes each
`--arg name value` as a GraphQL variable:

```bash
gql() {
  local query=$1; shift
  jq -n --arg query "$query" "$@" '{query: $query, variables: ($ARGS.named | del(.query))}' \
    | curl -s localhost:9292/graphql -H 'Content-Type: application/json' -d @-
  echo
}
```

**1. Onboard an entity.** Go provisions its Holder, plus one Wallet per account
type: bank, bank control, cash, uncleared cash, and so on.

```bash
ENTITY=$(gql 'mutation { onboardEntity(name: "Acme") { entity { id } } }' \
  | jq -r '.data.onboardEntity.entity.id')
```

**2. Initiate a $125.00 deposit.** Go records the Transaction and returns; it
is `INITIALIZED` and nothing has happened yet. The orchestrator picks it up and
stages the real leg (bank → cash). Once it is staged, the submission sweep hands
the entry to the fake ACH provider and confirms the leg, which then waits as
pending for the ACH network — within a minute, or immediately with
`submitAchNow`.

```bash
ACH=$(gql 'mutation($entity: ID!) {
  initiateAchDeposit(entityId: $entity, amountMinorUnits: "12500") { achTransaction { id } }
}' --arg entity "$ENTITY" | jq -r '.data.initiateAchDeposit.achTransaction.id')
```

**3. Watch the read model catch up.** State comes from the Ruby consumer. It is
`null` for a moment, then `INITIALIZED`, then `STARTED` with the real leg
`STAGED` and — once the sweep has run — `PENDING`. A Transaction that stays
`INITIALIZED` means the orchestrator is not running or the connector is not
registered.

```bash
achs() {
  gql 'query($entity: ID!) {
    achTransactions(entityId: $entity) { nodes { id direction amountMinorUnits state reason realLegState } }
  }' --arg entity "$ENTITY" | jq '.data.achTransactions.nodes'
}
achs
```

**4. Settle it.** This stands in for the provider's "entry posted" notice. The
real leg commits, the shadow leg (bank control → uncleared cash) runs, and the
Transaction completes: `COMPLETED`, with the real leg `COMMITTED`.

```bash
# Don't wait a minute for the sweep:
gql 'mutation($ach: ID!) { submitAchNow(achTransactionId: $ach) { achTransaction { id } } }' --arg ach "$ACH"
sleep 3 && achs

gql 'mutation($ach: ID!) { settleAch(achTransactionId: $ach) { achTransaction { id } } }' --arg ach "$ACH"
sleep 3 && achs
```

**Or return it instead.** Start another deposit (step 2) and report an ACH
return in place of step 4. The real leg is cancelled and Go rolls the
Transaction back: `ROLLED_BACK`, with the reason.

```bash
gql 'mutation($ach: ID!) {
  returnAch(achTransactionId: $ach, reason: "R01 insufficient funds") { achTransaction { id } }
}' --arg ach "$ACH"
sleep 3 && achs
```

`make consumer-logs` shows each event as it is projected.

**5. Clearing.** A settled deposit's money stays in uncleared cash until the
start of its third Federal Reserve business day; `achs` shows that date as
`clearingDueOn`. With `make jobs-up` running, the scheduled sweep then clears
it and `clearingState` becomes `COMPLETED`. To see it without waiting, run the
sweep with a later clock — see [docs/ach-transactions.md](docs/ach-transactions.md#clearing).

Withdrawals (`initiateAchWithdrawal`) take the same steps and draw on cleared
cash, so they complete once a deposit has cleared, and roll back before then.

When you are done, tear the pipeline down before the stack:

```bash
make jobs-down consumer-down orchestrator-down
make cdc-down
make down
```

### Running services outside Docker

```bash
# Go backend
cd go && go run ./cmd/server

# Ruby backend (expects the Go backend and Postgres reachable)
cd ruby && bin/server

# Client (expects the Ruby backend reachable, see client/README.md)
cd client && npm install && npm run dev
```

### Regenerating protobuf code

After changing anything under `proto/`:

```bash
bin/generate_protos.sh
```

## Testing & linting

Tests are written first, then code to pass them. Run these before pushing:

```bash
# Go
cd go && go test ./...
cd go && golangci-lint run

# Ruby
cd ruby && bundle exec rspec
cd ruby && bundle exec rubocop
cd ruby && bundle exec srb tc       # Sorbet type check

# Client
cd client && npm run build          # vue-tsc type check + Vite build
```

CI (`.github/workflows/`) runs Go tests, Ruby specs, Rubocop, and Sorbet's
`srb tc` against every push/PR to `main`.

## Conventions

See [CLAUDE.md](CLAUDE.md) for coding conventions: test-first development,
no linter-disable comments, avoiding `T.untyped` in Sorbet, and the
`# typed: strict` default for new Ruby files.
