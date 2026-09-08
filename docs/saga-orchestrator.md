# Running the saga orchestrator locally

The event-triggered saga orchestrator consumes `transfer-events` and `transaction-events` and drives the
Transfer and Transaction sagas forward. The code is [`go/internal/saga`](../go/internal/saga) and
[`go/cmd/orchestrator`](../go/cmd/orchestrator); the decisions behind it are
[`go/docs/adr/0001`](../go/docs/adr/0001-event-triggered-saga-orchestrator.md) (a message is a trigger, never
data), [`0002`](../go/docs/adr/0002-asynchronous-rollback-and-reversal-reconciliation.md) (rollback is a wait
state) and [`0003`](../go/docs/adr/0003-orchestrator-failure-handling.md) (a message it cannot process halts
it).

**It does not replace the synchronous saga.** Every RPC handler in `transfer` and `transaction` still runs its
own saga in process, exactly as before. The orchestrator is a second driver of the same sagas, and is a no-op
wherever the first one already did the work. That is what makes it safe to run before the cutover, and it is
asserted directly in `TestOrchestrator_IsANoOpWhenTheSynchronousPathAlreadyFinished`.

## What it does with a message

Read the aggregate id off the key, the aggregate type off the body, and then go and look at Postgres:

| Topic                 | What it drives                                                                       |
| --------------------- | ------------------------------------------------------------------------------------ |
| `transaction-events`  | that Transaction's saga.                                                               |
| `transfer-events`     | that Transfer's saga, **and then the Transaction that owns it**, if any.               |

The second half is the part that is easy to leave out and fatal to leave out. A trigger on `transfer-events`
names a Transfer, but "have all my children finished?" and "has this Reversal resolved?" are decisions on the
Transaction. Without following the link, a Transaction whose last child just committed would sit in `started`
forever — the liveness stall ADR 0001 measured. `transfer.OwningTransaction` is what follows it, reading the
`transaction_id` the Accepted event already records. A Reversal carries the same field, which is what lets a
rollback finish at all.

The published `payload` is never decoded. It is not merely unused: the envelope this package parses has no
field it could land in, and `TestPackageNeverDecodesPayload` fails the build if any source file in the package
so much as names it.

## Running it

The orchestrator reads the topics the CDC connector publishes, so the connector has to exist first. It is
behind the `cdc` compose profile for that reason — `docker compose up -d` does not start it, because without
`make cdc-up` there would be nothing on the topics at all.

```bash
make cdc-up
```

```bash
make orchestrator-up
```

```bash
make orchestrator-logs
```

It consumes the scratch `money_flow_cdc` database the tracer bullet publishes, never `money_flow_dev`: the
connector is registered against that database (`docker/cdc/events-connector.json`), and nothing publishes
development's event log. To exercise it end to end, run an RPC server against the same scratch database and
drive it over Twirp's JSON transport:

```bash
docker compose run --rm --service-ports -e DATABASE_URL="postgres://money_flow:money_flow@postgres:5432/money_flow_cdc?sslmode=disable" go go run ./cmd/server
```

The shape worth watching is a Transaction with a **staged** child, because that is the one the synchronous path
cannot finish on its own:

1. `StartInitializingTransaction` dispatches the staged child, which parks at `staged`. The Transaction parks
   at `started`.
2. `ConfirmStagedTransfer` and then `PostPendingTransfer` on the child settle it. Neither runs the
   Transaction's saga — nothing tells the Transaction anything.
3. The child's `TransferCommitted` reaches `transfer-events`, the orchestrator resolves it to its owning
   Transaction, and the Transaction reconciles the child, dispatches whatever the child was blocking, and
   completes.

Step 3 is the whole point: before this, only an incidental later RPC — `ResumeTransaction`, called by hand —
would have moved it.

```bash
make cdc-down
```

Tear the connector down before `docker compose down`, exactly as [`docs/cdc-tracer-bullet.md`](cdc-tracer-bullet.md)
describes: the replication slot outlives the stack and pins WAL until something drops it.

## Watching it

- **Consumer groups.** `money-flow-saga-transfer` and `money-flow-saga-transaction`, one per topic, per root
  ADR 0001 decision 6. Separate offsets, separate lag, separate blast radius.
- **Lag, not silence.** An event log is legitimately idle for long stretches, so "nothing is happening" is not
  a signal. Watch group lag and watch for the process exiting.
- **A halt is loud and terminal.** The orchestrator logs the exact topic, partition and offset it stopped on
  and exits non-zero, having committed nothing past that message. Restarting resumes on the same message, which
  is deliberate: ADR 0003 chooses stopping over skipping, because a skipped trigger is the only wake-up its
  aggregate was going to get.

## What a run actually looks like

**Ran 2026-09-08, on the local development stack**, following the steps above exactly: connector registered,
orchestrator started before anything had been published, then the ACH-shaped Transaction driven through an RPC
server pointed at `money_flow_cdc`.

The Transaction parks after `StartInitializingTransaction`, with its staged child waiting on settlement:

```
1|transaction.v1.TransactionInitialized
2|transaction.v1.TransactionStarted
3|transaction.v1.TransferRequestedWithinTransaction
```

`ConfirmStagedTransfer` and `PostPendingTransfer` then settle the child. Neither touches the Transaction — and
this is what the orchestrator did with that, unprompted:

```
saga: handled transfer-events[0]@4: transfer bbbb…bbbb (transfer.v1.TransferCommitted seq 5, global_seq 22)
saga: handled transaction-events[2]@3: transaction aaaa…aaaa (transaction.v1.TransferCompletedWithinTransaction seq 4, global_seq 23)
saga: handled transfer-events[1]@0: transfer cccc…cccc (transfer.v1.TransferRequestAccepted seq 1, global_seq 24)
saga: handled transaction-events[2]@4: transaction aaaa…aaaa (transaction.v1.TransferRequestedWithinTransaction seq 5, global_seq 35)
saga: handled transfer-events[1]@2: transfer cccc…cccc (transfer.v1.TransferCommitted seq 3, global_seq 34)
saga: handled transaction-events[2]@6: transaction aaaa…aaaa (transaction.v1.TransactionCompleted seq 7, global_seq 37)
```

```
4|transaction.v1.TransferCompletedWithinTransaction   -- reconciled off the transfer topic
5|transaction.v1.TransferRequestedWithinTransaction   -- shadow leg, unblocked by the child completing
6|transaction.v1.TransferCompletedWithinTransaction
7|transaction.v1.TransactionCompleted
```

Nothing but the orchestrator could have written those four events. The last RPC anyone made was
`PostPendingTransfer` on the child.

**A defect this run found, which no unit test would have.** The first attempt consumed nothing at all and only
started working after a restart. A consumer group that forms while its topic does not yet exist is assigned no
partitions — correct, and what every Kafka client does — but kafka-go's partition watcher, the thing meant to
notice the topic appearing, gives up permanently in exactly that case. The reader then sits in a
healthy-looking generation reading nothing. Since the topics here are created by the connector's first
publication, that was the *normal* startup order, not a corner case. `kafkareader.WaitForTopic` now blocks
until the topic exists before the group forms, and says so while it waits.

## A note on the tracer bullet's own messages

`go/cmd/cdctracer` appends synthetic events — a `TransferRequestAccepted` naming no wallets, for instance — to
produce the evidence in [`docs/cdc-tracer-bullet.md`](cdc-tracer-bullet.md). Those are not drivable Transfers,
so an orchestrator consuming a topic that still holds them halts on the first one. That is correct, and running
the tracer against a live orchestrator is the cheapest way to see ADR 0003's policy actually fire:

```
saga: retrying transfer-events[1]@3 (transfer 01a080f5-… (transfer.v1.TransferRequestAccepted seq 1, global_seq 38)), attempt 2 of 3, after: twirp error internal: ERROR: invalid input syntax for type uuid: "" (SQLSTATE 22P02)
saga: retrying transfer-events[1]@3 …, attempt 3 of 3, after: …
saga: HALTED on transfer-events[1]@3; nothing further will be consumed from this reader: saga: halted on transfer-events[1]@3 after 3 attempt(s): …
orchestrator: saga: halted on transfer-events[1]@3 after 3 attempt(s): …
exit status 1
```

Bounded retries, then a stop that names the exact message, commits nothing past it, and exits non-zero. A run
of the tracer and a run of the orchestrator therefore want different topic contents; `docker compose down`
discards the topics (the `kafka` service has no volume), which is the simplest way to start from a clean one.
