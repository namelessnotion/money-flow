# ACH Transactions end to end

An ACH deposit or withdrawal is originated from GraphQL, run by Go as a two-leg Transaction, and observed back
in Ruby through the read-model consumer. The vocabulary is in [`ruby/CONTEXT.md`](../ruby/CONTEXT.md). The
consumer's decisions are in [`ruby/docs/adr/0001`](../ruby/docs/adr/0001-read-model-consumer-and-follow-on-write-back.md)
and [`0002`](../ruby/docs/adr/0002-projection-consumer-topology-and-halt-policy.md).

```
GraphQL ──► Services::Ach::Initiate ──► Go: StartInitializingTransaction   (real leg stops, staged)
                    │                    Go: ConfirmStagedTransfer         (after provider submission)
                    ▼
            ach_transactions (intent, ids)

settleAch ──► Go: PostPendingTransfer, ResumeTransaction   (shadow leg runs; completed)
returnAch ──► Go: CancelStagedTransfer, ResumeTransaction  (rolled back)

Go events ──► money_flow_dev.events ──► Debezium ──► transfer-events / transaction-events
          ──► ruby/bin/consumer ──► transaction_projections / transfer_projections ──► GraphQL state
```

There is no real ACH provider yet. `Services::Ach::FakeProvider` accepts every submission, and the
`settleAch` / `returnAch` mutations stand in for the provider's settlement and return notices.

## Running it

The consumer only sees events once `money_flow_dev` is published, which is what `make cdc-up` does: it
registers the Debezium connector (`docker/cdc/events-connector.json`). Publication starts from the current end
of the log, so earlier events are never published. The Go orchestrator reads the same topics and can run
alongside (`make orchestrator-up`); it resumes Transactions on its own, which `settleAch` / `returnAch` also
do, idempotently.

```bash
docker compose up -d
make cdc-up
make consumer-up
make consumer-logs
```

Drive it over GraphQL at `http://localhost:9292/graphql`:

```graphql
mutation { onboardEntity(name: "Acme") { entity { id } } }
mutation { initiateAchDeposit(entityId: "1", amountMinorUnits: "12500") { achTransaction { id } } }
query    { achTransactions(entityId: "1") { nodes { id direction state reason realLegState } } }
mutation { settleAch(achTransactionId: "…") { achTransaction { id } } }
mutation { returnAch(achTransactionId: "…", reason: "R01 insufficient funds") { achTransaction { id } } }
```

The client does the same from an entity's page (`/entities/:id`): it lists the entity's accounts and ACH
Transactions, starts a deposit or withdrawal, and links each Transaction to a page showing its `steps`, with
Settle / Return buttons standing in for the provider while settlement waits.

A freshly initiated deposit shows `state: null` until the consumer catches up. After that it shows `STARTED`
with the real leg `PENDING`. `settleAch` takes it to `COMPLETED`; `returnAch` takes it to `ROLLED_BACK`.

To watch what Go does as it happens, run `make events` in another terminal: it prints every event appended
to the log, one per line, with its aggregate, stream position and decoded payload. `make events
ARGS="-types transaction,transfer"` narrows it to the saga, `ARGS="-id <fragment>"` to one aggregate, and
`ARGS="-follow <transaction id>"` to one Transaction with its Transfers, Operations and Reversals (from its
first event), and `ARGS="-json"` gives one JSON object per event for `jq` (`go run ./cmd/events -h` lists the rest). It reads
Postgres directly, so it works without `make cdc-up`.

Tear down in this order, before `docker compose down`. The replication slot outlives the containers and pins
WAL if left behind.

```bash
make consumer-down
make cdc-down
```

## Clearing

A deposit's shadow leg leaves its money in `uncleared_cash`. From the start of the third Federal Reserve
business day after the deposit completed, a scheduled sweep originates a clearing Transaction that moves it to
`cleared_cash`, which is what withdrawals draw on
([`ruby/docs/adr/0003`](../ruby/docs/adr/0003-ach-clearing-as-a-scheduled-sweep.md)).

```
resque-scheduler (hourly, Mon-Fri, America/New_York)
  -> Jobs::ClearAchDeposits on resque-worker
     -> Services::Ach::ClearDue: completed deposits, 3+ business days old, clearing not yet seen
        -> Services::Ach::Clear: Go StartInitializingTransaction(id = detid("<ach id>:clearing"))
```

`achTransactions` shows `clearingDueOn` for a completed deposit and `clearingState` once the clearing has been
projected. Run the jobs with `make jobs-up`; `make clear-ach-now` enqueues a sweep immediately (it clears only
what is already due). To see a clearing without waiting three business days, run the sweep with a later clock:

```bash
docker compose exec resque-worker bundle exec ruby -e \
  'require "./lib/environment"; require "logger"
   p Services::Ach::ClearDue.new(logger: Logger.new($stdout)).call(now: Time.now + 7 * 86_400)'
```

That clears **every** completed deposit in `money_flow_dev` that would be due by then, not just one.

To clear **one** deposit now, use the **Clear now** button on its page in the client. It appears once the
deposit has completed and no clearing is under way. The button calls the `clearAch` mutation
(`Services::Ach::ClearNow`), which skips the return window but still refuses a deposit whose Transaction has not
completed:

```graphql
mutation { clearAch(achTransactionId: "…") { achTransaction { clearingState } } }
```

It is a demonstration control, like `settleAch` / `returnAch`. In production nothing should clear a deposit the
network could still return.

A withdrawal funds itself first: its shadow leg moves cleared cash to bank control, and only then is the real
leg staged and the entry submitted ([`ruby/docs/adr/0004`](../ruby/docs/adr/0004-ach-withdrawal-funds-before-it-leaves.md)).
On an entity whose deposits have not cleared yet, `initiateAchWithdrawal` is refused before any money moves,
and the record shows funding failed and the rollback done.
