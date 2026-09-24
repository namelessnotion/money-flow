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

The orchestrator reads the topics the CDC connector publishes, so the connector has to exist first. Registering
it is a separate step from starting the containers: `docker compose up -d` does start the orchestrator (the
`cdc` compose profile these docs described was removed in 0a3818a), but without `make cdc-up` there is nothing
on the topics for it to read and it simply idles.

**Both are required, not tuning.** Since the async cutover
([go ADR 0006](../go/docs/adr/0006-synchronous-dispatch-removed-from-the-rpc-surface.md)) nothing in the RPC
surface runs a saga: `cmd/server` records decisions and answers, and this process does the rest. Without it, or
without the connector, every Transaction stops at `initialized` and no money moves — quietly, since nothing
errors.

`make resume-open` is the way out when a trigger will never arrive: publication starts at the current end of the
log, so anything written before `make cdc-up` is never published at all, and the aggregate has no wake-up
coming. It drives everything still in flight through this same orchestrator, out of band.

```bash
make cdc-up
```

```bash
make orchestrator-up
```

```bash
make orchestrator-logs
```

It consumes `money_flow_dev`, the database the connector publishes (`docker/cdc/events-connector.json`), so
anything the running `go` server does reaches it — including an ACH Transaction started from GraphQL
([`docs/ach-transactions.md`](ach-transactions.md)). To exercise it by hand, drive the `go` server over
Twirp's JSON transport on `:8080`.

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

Tear the connector down before `docker compose down`: the replication slot outlives the stack and pins WAL
until something drops it.

## Watching it

- **Consumer groups.** `money-flow-saga-transfer` and `money-flow-saga-transaction`, one per topic, per root
  ADR 0001 decision 6. Separate offsets, separate lag, separate blast radius.
- **Lag, not silence.** An event log is legitimately idle for long stretches, so "nothing is happening" is not
  a signal. Watch group lag and watch for the process exiting.
- **A halt is loud and terminal.** The orchestrator logs the exact topic, partition and offset it stopped on
  and exits non-zero, having committed nothing past that message. Restarting resumes on the same message, which
  is deliberate: ADR 0003 chooses stopping over skipping, because a skipped trigger is the only wake-up its
  aggregate was going to get.

## Recovering from a halt

A halt means a handler failed on the same message three times. Restarting the orchestrator does nothing
until whatever made it fail has changed: it resumes on that same message and halts again. So the job is to
find the cause and fix it, then let the message redeliver. **Do not reset or skip the offset.** A skipped
trigger strands its aggregate for good (ADR 0003), and the fix never needs it, because redelivery re-folds
authoritative state.

1. **Read the halt.** The last log line names the message and the error:

   ```bash
   docker logs money_flow-orchestrator-1 2>&1 | grep -i halt | tail -3
   ```

2. **Confirm which partition is stuck.** Lag sits on one partition, with the committed offset equal to the
   halted message:

   ```bash
   docker exec money_flow-kafka-1 /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --describe --group money-flow-saga-transfer
   ```

3. **Read the aggregate's stream.** Use psql, not `cmd/events`, which follows forever. A trail of repeated
   `*Started` claim markers with no outcome after them means the saga step is being retried and failing
   before it can record anything:

   ```bash
   docker exec money_flow-postgres-1 psql -U money_flow -d money_flow_dev -c "select sequence, event_type, occurred_at from events where aggregate_type='transfer' and aggregate_id='<id>' order by sequence"
   ```

4. **Fix the cause and deploy it.** The orchestrator compiles the main checkout's `go/` when it starts, so a
   merged fix reaches it on the next start.

5. **Drive the aggregate, then restart the orchestrator.** `make resume` compiles the current tree on each run
   and drives the aggregate, and through it the Transaction that owns it, out of band. It changes no offsets.
   Restarting afterwards redelivers the halted message. The saga finds the step already done and does
   nothing, commits the offset, and the partition drains.

   ```bash
   make resume ARGS="<aggregate id>"
   ```

   ```bash
   make orchestrator-up
   ```

A stale claim marker left by the failed attempts does not block this. Once it is older than `claimStaleAfter`
(5s), the same transition takes it over (go ADR 0005).

### 2026-09-23: `transfer-events[3]@88902`, too much data

- **Halt:** `ledger: CreateTransfers: too much data was sent or requested in this batch`, twice (23:55 and
  00:03). The orchestrator exited, and `money-flow-saga-transfer` partition 3 was held at 88902 with a lag of 8.
- **Aggregate:** Transfer `d0803d1c-090d-4565-ac2b-eb5e76564a85`, a 276-leg security draw. Its stream is
  `TransferPrepared` followed by six `TransferCommittingStarted` markers. Its Transaction
  `bf65e369-6611-4949-8d4f-69ec1762b4dc` is parked at `TransferRequestedWithinTransaction`.
- **Cause:** one linked batch of 276 legs, against a `--development` TigerBeetle that carries 253 per
  request. The client refuses the request locally, so nothing reached the ledger, and there is nothing to
  undo ([go ADR 0008](../go/docs/adr/0008-transfer-legs-span-ledger-batches.md)).
- **Recovery:** merge the ADR 0008 change, then run step 5 with the Transfer id. The Transfer reserves and
  posts in two chains and records `TransferCommitted`, and its Transaction carries on. If the escrow's Tokens
  have been spent since, the reservation is refused instead: the Transfer records `TransferFailed` and the
  Transaction rolls back. Either way the partition drains.

## What a run actually looks like

**Ran 2026-09-08, on the local development stack**, following the steps above exactly: connector registered,
orchestrator started before anything had been published, then the ACH-shaped Transaction driven through an RPC
server pointed at the scratch `money_flow_cdc` database that the connector published at the time. It has
since been retired in favour of publishing `money_flow_dev`; the behaviour shown is unchanged.

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

Each topic is waited for in its own goroutine, not one after another. Sequentially, a topic nothing has
published to yet would hold up every consumer behind it, and after the cutover that is a deadlock rather than a
delay: `transfer-events` does not exist until a Transaction dispatches its first child, and the only thing that
can make it do so is a `transaction-events` trigger the orchestrator would be blocked from consuming.

The wait is unbounded for a *missing topic* and bounded for a *missing cluster*, because only one of those
arrives on its own. A topic the connector has not published to yet appears without anyone doing anything, so
waiting for it forever is right. A cluster nothing in `KAFKA_BROKERS` answers does not: left unbounded, a
misconfigured address produces a process that logs a dial failure every two seconds and consumes nothing while
looking perfectly healthy. After a minute with no broker reachable the wait fails, which stops the
orchestrator with an error naming every address it tried. Each poll asks the brokers in turn and takes the
first answer — any broker serves metadata for the whole cluster, so a multi-broker `KAFKA_BROKERS` survives
one of them being down or restarting rather than being no more available than a single-broker one.

## A note on synthetic events

The CDC tracer bullet's `go/cmd/cdctracer` (since removed, with the scratch database it wrote to) appended
synthetic events — a `TransferRequestAccepted` naming no wallets, for instance. Those are not drivable
Transfers, and running them past a live orchestrator is how ADR 0003's policy was first seen to fire:

```
saga: retrying transfer-events[1]@3 (transfer 01a080f5-… (transfer.v1.TransferRequestAccepted seq 1, global_seq 38)), attempt 2 of 3, after: twirp error internal: ERROR: invalid input syntax for type uuid: "" (SQLSTATE 22P02)
saga: retrying transfer-events[1]@3 …, attempt 3 of 3, after: …
saga: HALTED on transfer-events[1]@3; nothing further will be consumed from this reader: saga: halted on transfer-events[1]@3 after 3 attempt(s): …
orchestrator: saga: halted on transfer-events[1]@3 after 3 attempt(s): …
exit status 1
```

Bounded retries, then a stop that names the exact message, commits nothing past it, and exits non-zero. Never
append hand-made events to `money_flow_dev`: the log is append-only, so they would halt the orchestrator and
the Ruby consumer on every replay, permanently.
