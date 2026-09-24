<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
  <img alt="MoneyFlow" src="assets/logo-light.svg" height="72">
</picture>

Event sourced tokenized transaction system

MoneyFlow moves money between the parties of a marketplace and can prove
where every cent went. It is split into two backends with one job each:

- **Go records what money is meant to do and does it.** Every command becomes
  an immutable event in an append-only PostgreSQL log, and sagas turn those
  events into double-entry token movements in
  [TigerBeetle](https://tigerbeetle.com). Nothing is updated in place, and
  every balance can be rebuilt from the log.
- **Ruby decides what the business wants.** Onboarding, ACH deposits and
  withdrawals, and a real-estate lending market (Securities that Investors buy
  in fractions, draw to Borrowers, and repay with interest) are orchestrated
  here and exposed over GraphQL.

A Vue client renders the result, including a graph that replays every
Movement of money in the order it completed.

[![Watch the money flow graph replay a simulated lending market](https://img.youtube.com/vi/LfLltacbiTA/maxresdefault.jpg)](https://www.youtube.com/watch?v=LfLltacbiTA)

_The money flow graph replaying a [lending market simulation](#market-simulation-a-lending-market)
([watch on YouTube](https://www.youtube.com/watch?v=LfLltacbiTA)): Investors →
Securities → Borrowers, and back with interest._

## Status

Early, but money moves end to end through the real stack:

- Onboarding an `Entity` in Ruby provisions a `Holder` and its `Wallet`s in Go
  over Twirp, recorded as immutable events. Each entity has a Role: `investor`,
  `borrower` or `issuer`.
- An **ACH deposit or withdrawal** started from GraphQL runs in Go as a two-leg
  Transaction: a staged real leg that waits on the ACH network, and a shadow
  leg that follows it. Settlement and returns are reported through GraphQL,
  standing in for an ACH provider. Deposits clear on a scheduled sweep.
- **Securities** go through their whole life: an Issuer offers one, Investors
  subscribe in fractions (the ledger itself refuses oversubscription), it is
  drawn to its Borrower, repaid with simple interest, and disbursed pro rata
  to its holders. Each step is its own Go Transaction.
- Go's events are published to Kafka by Debezium (CDC). The Go orchestrator
  drives sagas forward from them, and a Ruby consumer folds them into a read
  model that GraphQL serves.
- Two simulations exercise all of this: a **benchmark** that loads the Go
  backend and checks the ledger balances afterwards, and a **lending market**
  that plays out months of Investor and Borrower activity through GraphQL's
  own services and replays it as a graph.

See [docs/ach-transactions.md](docs/ach-transactions.md) for the ACH flow and
[ruby/CONTEXT.md](ruby/CONTEXT.md) for the securities vocabulary.

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
([docs/saga-orchestrator.md](docs/saga-orchestrator.md)); `cmd/simulate` is the
[benchmark](#benchmark-simulation-go-under-load). Decisions:
[go/docs/adr](go/docs/adr).

### `ruby/`

Business backend. Exposes GraphQL (`app/graphql`) backed by Sequel models
(`app/models`) over PostgreSQL, and orchestrates money-flow operations
(`app/services`) by calling the `go/` backend over Twirp. Runs on Falcon,
reachable directly on `:9292` or via the proxy at
`https://graphql.local.namelessnotion.com`. `bin/consumer` reads the published
events into a lagging read model (`app/consumer`,
[ruby/docs/adr](ruby/docs/adr)). `bin/simulate_lending` runs the
[market simulation](#market-simulation-a-lending-market). Vocabulary:
[ruby/CONTEXT.md](ruby/CONTEXT.md).

### `client/`

Vue 3 + TypeScript SPA. Apollo Client (via `@vue/apollo-composable`) queries
the Ruby GraphQL API; TailwindCSS for styling. Pages for entities and their
ACH Transactions, and `/money-flow`, the replayable graph of where money went
(Cytoscape). Served by Vite, reachable directly on `:5173` or via the proxy at
`https://app.local.namelessnotion.com`.

## Running locally

In short, from a clean checkout:

```bash
make up                                          # the stack
make cdc-up orchestrator-up consumer-up jobs-up  # the event pipeline: without it no money moves
```

Then open the client at `http://localhost:5173` (or
`https://app.local.namelessnotion.com`, see below). The rest of this section
explains each piece.

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

## Simulations

Both simulations drive the real, running stack, so bring it up **with the
whole event pipeline** first (`make up`, then `make cdc-up orchestrator-up
consumer-up`). If everything they start stays open, check
`make orchestrator-logs` before anything else.

Both write to the development database, and the event log is append-only:
what a run writes stays. Entities from different runs never mix, because
each run creates its own.

### Benchmark simulation: Go under load

[`go/cmd/simulate`](go/cmd/simulate/main.go) measures the Go backend on its
own, over Twirp, with Ruby out of the loop. It provisions `-entities` entities,
seeds each from a reserve, then drives `-transactions` transfers between
random pairs, `-concurrency` at a time, steering `-rollback-rate` of them to
roll back instead of settle. Each one counts as finished only once the
orchestrator has taken it to a terminal state, so the latency it reports is
the whole trip through CDC, Kafka and the orchestrator, not just an RPC.

`make simulate` runs it in the `go` container. It links TigerBeetle's native
client, which does not link on macOS hosts:

```bash
make simulate                                                              # 200 transfers across 20 entities
make simulate ARGS="-entities 150 -transactions 2000 -concurrency 32 -seed 42"
```

| Flag | Default | |
| --- | --- | --- |
| `-mode` | `transaction` | `transaction` wraps every Transfer in a single-child Transaction, the production ACH shape. `transfer` drives `TransferService` directly, to measure Transfers without Transaction dispatch |
| `-entities` | `20` | Wallets to spread the load over. Keep it well above `-concurrency`, or a few hot Wallets contend |
| `-transactions` | `200` | How many to drive |
| `-concurrency` | `8` | How many are in flight at once |
| `-rollback-rate` | `0.3` | Share steered to roll back |
| `-seed` | time | Fix it to repeat a run |
| `-skip-verify` | `false` | Skip the ledger check at the end |

`make simulate ARGS=-h` lists the rest (amounts, timeouts, how long to wait for stragglers).

It prints two reports. **Load**: throughput, p50/p95/p99 latency, and how
many ended in each terminal state. **Ledger correctness**: every entity's
balance, read back from the event log, matches what its transfers should have
left, and the total across entities equals what was seeded. Any mismatch is
printed as one.

Keep `-concurrency` below about 60. The tool gives itself one Postgres
connection per in-flight transfer, and past Postgres' `max_connections` of
100, "too many clients" errors are the harness failing, not the system. On a
Docker Desktop laptop, throughput peaks at around concurrency 48, at roughly
280 transfers/s in `transfer` mode and 215/s in `transaction` mode. The limit
there is Postgres syncing its WAL to Docker's disk, not TigerBeetle or the
orchestrator.

### Market simulation: a lending market

[`ruby/bin/simulate_lending`](ruby/bin/simulate_lending) plays out a
Groundfloor-like real-estate lending market on a compressed clock, one
simulated day at a time. Each day, Investors top up over ACH, new loans are
offered as Securities, Investors auto-invest in what is open, fully
subscribed Securities are drawn to their Borrowers (who withdraw the money),
loans that come due are repaid with interest and disbursed to their holders,
and now and then an Investor withdraws. Every step is a real Go Transaction,
started through the same Ruby services GraphQL uses, and the next step waits
for the read model to see the last one complete.

```bash
make simulate-lending                                   # seed 42: 40 Investors, 8 Borrowers
make simulate-lending ARGS="--seed 7 --investors 20"
```

| Option | Default | |
| --- | --- | --- |
| `--seed N` | `42` | The same seed plays the same market |
| `--investors N` | `40` | |
| `--borrowers N` | `8` | |
| `--issue-days N` | `180` | Days new loans are offered. The run then continues until every loan is repaid |
| `--max-days N` | `1500` | Hard stop, in simulated days |
| `--seconds-per-day S` | `0.1` | Least real time per simulated day |
| `--start-date DATE` | today | First simulated day, `YYYY-MM-DD` |
| `--profile PATH` | [`groundfloor_like.yml`](ruby/config/simulation/groundfloor_like.yml) | The market's shape |
| `--tag TAG` | `sim-<seed>-<time>` | Prefix for every entity name the run creates |

The profile holds the market's shape as decile tables: loan sizes, grades
and rates, terms, how early or late loans pay off, position sizes, and
deposit habits. It was calibrated from aggregate figures from a real
platform, then scaled so tens of Investors can fund a loan.

The run prints its tag first. At the end it reads the money flow back through
the read model, not its own bookkeeping, and logs the total of each kind of
Movement along with three checks: every Repayment was disbursed in full,
every Draw equals the Subscriptions that funded it, and every entity's
cleared cash and cash on the ledger match what the run expected. It exits
non-zero if any check fails. The ACH, clearing and disbursement sweeps are
called directly for each step, so `make jobs-up` is not needed.

### Replaying a run as a graph

Open the tag the run printed in the client:

```
http://localhost:5173/money-flow?run=<tag>
https://app.local.namelessnotion.com/money-flow?run=<tag>
```

Without `?run=`, the page shows every Movement from every run. Each Party
(Investor, Security, Borrower, or the Bank) is a node, and each completed
Movement is an edge coloured by which way the money is going. **Play**
replays the Movements in the order Go completed them, and the slider scrubs
through time. The checkboxes filter by flow (investing, repaying, interest,
bank deposits and withdrawals) or collapse each Security into its Borrower.
Click a Party to follow its money, and hover an edge to see its amount and
time. [The video above](https://www.youtube.com/watch?v=LfLltacbiTA) shows a
full run replaying.

## Development

### Running services outside Docker

```bash
# Go backend
cd go && go run ./cmd/server

# Ruby backend (expects the Go backend and Postgres reachable)
cd ruby && bin/server

# Client (expects the Ruby backend reachable, see client/README.md)
cd client && npm install && npm run dev
```

Anything that links TigerBeetle's native client (`cmd/server`,
`cmd/orchestrator`, `cmd/simulate`, and most of the Go tests) fails to link on
macOS hosts. Run those in the `go` container instead.

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
cd client && npm test               # Vitest + Vue Test Utils
cd client && npm run build          # vue-tsc type check + Vite build
```

With the stack up, the Go and Ruby suites run in their containers, which have
the native libraries and gems they need and point at the `money_flow_test`
database:

```bash
docker compose exec -T go go test ./...
docker compose exec -T ruby bundle exec rspec
```

CI (`.github/workflows/`) runs Go tests, golangci-lint, Ruby specs, Rubocop,
and Sorbet's `srb tc` against every push/PR to `main`.

## Conventions

See [CLAUDE.md](CLAUDE.md) for coding conventions: test-first development,
no linter-disable comments, avoiding `T.untyped` in Sorbet, and the
`# typed: strict` default for new Ruby files.
