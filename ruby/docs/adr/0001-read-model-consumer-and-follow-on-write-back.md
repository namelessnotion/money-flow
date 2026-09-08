# 1. Ruby as a lagging read-model consumer, with deterministic-id write-back

- **Status:** Accepted
- **Date:** 2026-09-07
- **Scope:** `ruby/` context only.
- **See also:** [root ADR 0001](../../../docs/adr/0001-async-event-driven-saga-and-postgres-to-kafka-publication.md),
  [`go/docs/adr/0001`](../../../go/docs/adr/0001-event-triggered-saga-orchestrator.md)

## Context

Ruby has no visibility into Transfer or Transaction state today. Root ADR 0001 publishes Go's domain events to
`transfer-events` and `transaction-events`; Ruby consumes both to maintain its own projection and to react.

Ruby cannot take the event-as-trigger reading that `go/docs/adr/0001` adopts for the orchestrator. That reading
works only because the orchestrator sits beside the authoritative event store and can re-read it. Ruby is a
separate bounded context with no access to Go's Postgres — **the topic really is its only channel**, so it has
no authoritative state to re-fold and must fold the message payload.

That asymmetry is the whole reason this ADR exists separately.

## Decision

1. **Ruby is a CQRS-style read-model consumer and is allowed to lag.** Eventual consistency is the contract.
   Lag is normal; disagreement is a defect.
2. **The projector applies a monotonic guard**, not bare last-write-wins: an event is applied only if its
   sequence for that aggregate is greater than the last one applied. Persist the high-water mark alongside the
   projection.
3. **Ruby projects every terminal state, not only the ones on the happy path.** That includes
   `TransferRequestRejected` and `TransactionRejected` — which never advance past an aggregate's first event and
   so have no place in the saga's own progression — and `TransactionRollbackFailed`. It also includes Reversal
   aggregates, which arrive on `transfer-events` like any other Transfer (see
   [`go/docs/adr/0002`](../../../go/docs/adr/0002-asynchronous-rollback-and-reversal-reconciliation.md)) and
   whose ids Ruby has never seen before.

   **This is not hypothetical.** Building the rollback model, `TransactionRollbackFailed` was left out of the
   projection's event→state map. The read model silently ignored it: Ruby would have shown a Transaction stuck
   in `rollback_started` forever while Go had long since recorded `rollback_failed`. Nothing failed loudly — the
   agreement invariant caught it only because it checks for disagreement once nothing is left to deliver. A
   projector that drops an unrecognised event type is indistinguishable from one that is merely behind. **Make
   an unmapped event type a loud error, not a silent skip.**
4. **Follow-on write-back uses a deterministic derived id, and relies on Go for idempotency** — not on Ruby-side
   bookkeeping. Derive the id from the triggering aggregate, mirroring `detid.New(...)`, and call Go normally.

## Evidence

From the prototype in [`prototype/`](../../../prototype/prototype-async-saga-state-model.md).

**Why the monotonic guard.** Last-write-wins is idempotent under *duplication* — applying "this aggregate is now
in state X" twice changes nothing — and defenceless under *reordering*. The fuzzer found this unprompted in 54
steps, under CDC with nothing dropped and nothing crashed: two of one Transfer's own messages arrive swapped,
Ruby walks `prepared → accepted`, and **never recovers**. Go read `committed`; Ruby read `prepared`, with
nothing left to consume.

Root ADR 0001's `aggregate_id` partition key plus producer idempotence should make same-aggregate reordering
impossible at the transport layer. The guard is kept anyway, because it is nearly free and the transport
guarantee is easy to lose silently: a repartition, a key change, a producer misconfiguration, or a future
migration to a different bus removes it with no failing test. Defence in depth on a cheap invariant.

**Why the write-back guard belongs in Go.** `StartInitializingTransaction` already loads by id and returns the
recorded decision as-is when a stream exists. So a deterministic id makes double origination a no-op **even if
Ruby forgets everything** — a consumer-group reset, a redeploy, a replayed topic. A Ruby-side "already
originated" set would be a weaker guard in a place that cannot enforce it. The prototype modelled the Ruby-side
version and thereby demonstrated the wrong mechanism; this ADR corrects that.

## Consequences

- **Ruby's projection is not a source of truth and must never be treated as one.** Anything needing authority
  reads Go through its Twirp API.
- **The high-water mark is real persisted state** with a migration and a backfill story, not an in-memory
  detail.
- **A follow-on Transaction originated by Ruby produces Go events that Ruby then consumes**, closing a
  Go → Kafka → Ruby → Go → Kafka → Ruby loop. Deterministic ids make each origination idempotent, but they do
  not by themselves bound a *chain* of distinct follow-ons. The prototype cut this loop off and never exercised
  it. Before shipping any second-order follow-on rule, establish what terminates the chain.
- **Ruby needs the generated protobuf classes** for the body fields the write-back reads, though not for the
  projection itself, which needs only `event_type` and `aggregate_id`.
- **Reversal aggregates appear unannounced.** Ruby learns of a Reversal only when its first event arrives, on a
  `transfer_id` no prior message mentioned. The projector must create the row on first sight rather than assume
  every aggregate was introduced by a Transaction it already knows about.

## Not decided here

- Which Ruby GraphQL types expose Transfer/Transaction state, and how lag is surfaced to `client/`.
- Consumer-group topology on the Ruby side, and whether projection and write-back share one consumer or split.
