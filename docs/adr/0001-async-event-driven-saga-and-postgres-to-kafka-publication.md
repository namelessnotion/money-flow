# 1. Async, event-driven saga with CDC publication from Postgres to Kafka

- **Status:** Accepted
- **Date:** 2026-09-07
- **Scope:** System-wide. Governs the transport contract between `go/` and `ruby/`.
- **Supersedes:** nothing. First ADR in this repo.
- **See also:** [`go/docs/adr/0001`](../../go/docs/adr/0001-event-triggered-saga-orchestrator.md),
  [`ruby/docs/adr/0001`](../../ruby/docs/adr/0001-read-model-consumer-and-follow-on-write-back.md)

## Context

`transfer.runSaga` and `transaction.runSaga` are in-process loops: each folds an aggregate's own event stream
into a state, dispatches one next step synchronously (calling TigerBeetle inline), appends the outcome event, and
repeats until it reaches a wait-state or a terminal state. That works, is idempotent by construction, and
self-heals by re-running from wherever the log left off.

Two things it cannot do:

1. **Ruby cannot react.** Ruby has no visibility into Transfer or Transaction state today — no projection, no
   polling, no messaging. It needs to update its own tracking objects, trigger communications, and sometimes
   originate a follow-on Transaction (auto-invest on an ACH posting). This is greenfield, not a migration.
2. **A halted system has no durable resume point.** The saga needs consumer offsets it can pick up from per
   aggregate type, rather than depending on a later RPC happening to touch the same id.

The `events` table already anticipates this: its `global_seq` index exists, per its own migration comment, so
"every projection/process manager reads forward from a bookmark on this index". It is unused today.

Nothing is in production, so this is a **hard cutover** — no strangler, no parallel run.

## Decision

1. **Publish domain events from the `events` table to Kafka.** Postgres remains the single source of truth.
   Kafka is transport and wake-up signal, never authority. Any consumer that needs to _know_ something reads
   Postgres; the topic only tells it when to look.
2. **Publish via CDC** (logical decoding, Debezium-style), **not a polling relay.**
3. **One topic per aggregate type** — `transfer-events`, `transaction-events` — routed by the existing
   `aggregate_type` column.
4. **Partition by `aggregate_id`.**
5. **Require producer idempotence** (`enable.idempotence=true`). Without it a producer retry can reorder records
   _within_ a partition, which would silently void decision 4.
6. **Process-manager orchestration**, one orchestrating consumer per aggregate type. Choreography is deferred
   until scale demands it.

## Evidence

From the throwaway prototype in [`prototype/`](../../prototype/prototype-async-saga-state-model.md), which modelled one Transaction with
two child Transfers (one staged, one not) against duplicate, reordered, delayed and dropped delivery. Five
invariants were checked after every action, and a seeded fuzzer searched for violations. 2,000 runs × 400 steps
per configuration:

| Configuration                                | Violations   | Stalls | Reached terminal |
| -------------------------------------------- | ------------ | ------ | ---------------- |
| **CDC / per-type / trigger / key=aggregate** | **0 / 2000** | **0**  | **2000 / 2000**  |
| …but polling relay                           | 1097 / 2000  | 0      | 1378             |
| …but unified topic                           | 0 / 2000     | 0      | 2000             |
| …but event-as-data                           | 0 / 2000     | 13     | 1945             |
| …but no partition key                        | 430 / 2000   | 0      | 1828             |

The prototype is kept in `prototype/` for reference, but it is throwaway and unmaintained — these numbers are
reproduced here so this decision stands on its own if it is ever deleted.

## Why not a polling relay

This ADR **reverses** the pre-build preference. The argument for polling was that `events.payload` is raw
protobuf `BYTEA` and Debezium's outbox router expects JSON, so unwrapping it would need a custom transform.

That argument mostly dissolves under the event-as-trigger handler adopted in `go/docs/adr/0001`, because neither
consumer's state-tracking path reads the payload at all:

- The orchestrator decodes nothing. A trigger only has to say _which aggregate moved_, and `aggregate_type` is a
  plain `TEXT` column while `aggregate_id` is `uuid`, which Debezium emits as a string.
- Ruby's projection needs `event_type` and `aggregate_id` — also plain columns. Debezium emits row columns as
  JSON with `payload` base64-encoded, so Ruby protobuf-decodes only when it wants body _fields_.

**Measured, not just reasoned.** This paragraph was the load-bearing, untested claim in this ADR, so it was
proven end to end before anything was built on it: see [`docs/cdc-tracer-bullet.md`](../cdc-tracer-bullet.md).
Debezium's bundled outbox `EventRouter` takes our column names as configuration — no custom transform, no extra
jars — and `table.fields.additional.placement` keeps `aggregate_type`, `event_type` and `sequence` in the
message body rather than burying them in headers behind an opaque payload. The cost argument below stands.

**This ADR therefore depends on `go/docs/adr/0001`.** If the orchestrator ever reverts to folding message
payloads, revisit this decision: the transform cost comes back.

Meanwhile polling has a failure mode that no other knob mitigates. `global_seq` is
`BIGINT GENERATED ALWAYS AS IDENTITY`: values are allocated at INSERT and become visible at COMMIT. A relay
reading `WHERE global_seq > bookmark ORDER BY global_seq` will therefore **permanently and silently skip** any
event whose sequence was allocated before, but committed after, one it has already read. No restart, no
incident, just two concurrent transactions. A partition key cannot help — the message is never published at all.

Mitigations exist (a lag window, `pg_snapshot_xmin(pg_current_snapshot())` tracking, or a `published_at` outbox
flag instead of a cursor), but each re-introduces cost or latency, and none was cheaper than adopting CDC.

## Why per-aggregate-type topics

The prototype measured this as **correctness-neutral**: a unified topic scored identically. This decision rests
on its original arguments alone — mirroring the `aggregate_type` column, and matching what Debezium's outbox
router does when one table serves several aggregate types — not on anything the prototype demonstrated. Recorded
explicitly so a future reader does not infer evidential support that does not exist.

One real consequence: two topics are independent partitions whatever you key on, so per-type topics cannot give
total per-transaction ordering. `go/docs/adr/0001` is what makes that acceptable.

## Why `aggregate_id` and not `transaction_id`

Keying by transaction id measured identically (0 / 2000) and costs more: one partition per Transaction is also
one consumer thread per Transaction. `aggregate_id` is the weakest sufficient key. Revisit only if concurrent
appends to a single Transaction's stream become a measured problem rather than a theoretical one.

## Consequences

**Accepted costs**

- **`wal_level = logical`.** The stack currently runs `replica`. This is a Postgres restart locally, and a
  parameter-group change plus reboot in every Terraform-managed environment.
- **A replication slot, and the on-call discipline it brings.** A stalled or deleted connector either pins WAL
  until the disk fills or silently loses its position. This is a materially different operational surface from a
  polling worker that can be restarted with impunity. Slot lag needs monitoring and an owner.
- **Net-new self-hosted infrastructure**: Kafka and Kafka Connect via Docker locally, Terraform for real
  environments. Not an existing platform being extended.
- **The publisher becomes a total-stall point.** Today a stalled relay is impossible; afterwards nothing
  advances without publication. Availability of the CDC pipeline is now a system-level concern.

**Gained**

- Durable, resumable per-consumer offsets.
- Ruby can react to Go's domain events without Go knowing Ruby exists.
- Eventual consistency, which matches the event-sourced model already in place.

**Explicitly not decided here**

- Retention, compaction and replication factor for the two topics.
- Schema/versioning policy for the published envelope, and whether `proto/` gains an explicit published-event
  contract distinct from the internal event types.
