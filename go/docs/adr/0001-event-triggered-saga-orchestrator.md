# 1. The saga orchestrator treats a delivered event as a trigger, not as data

- **Status:** Accepted
- **Date:** 2026-09-07
- **Scope:** `go/` context only.
- **See also:** [root ADR 0001](../../../docs/adr/0001-async-event-driven-saga-and-postgres-to-kafka-publication.md),
  which depends on this decision.

## Context

Root ADR 0001 moves saga orchestration onto Kafka. That leaves a question the transport decision cannot answer:
**when a message arrives, what is it?**

Two readings, both defensible:

- **Event-as-data** — the message *is* the fact. The handler folds its payload into state. This is the
  conventional stream-processing shape, and it makes the consumer self-contained.
- **Event-as-trigger** — the message means only "this aggregate moved". The handler ignores the payload and
  re-folds authoritative state from Postgres.

The existing code already takes the second reading. `reconcileInFlight` reads each child's live
`transfer.Outcome` rather than trusting anything handed to it, and `runSaga` loops load → dispatch → continue
until nothing progresses.

## Decision

**Event-as-trigger.** A consumed message causes the orchestrator to re-run the existing fold for the named
aggregate against Postgres. Its payload is never read.

Two corollaries, both load-bearing:

1. **Keep `appendSagaStep`'s write-side idempotency guard. Do not add a read-side processed-message table.**
   The guard that matters is "don't append this fact twice" (`transfer/saga.go`, and the `(type, transfer_id)`
   variant in `transaction/saga.go`), not "don't process this message twice".
2. **At-least-once delivery is sufficient.** Offsets may be committed after side effects. Redelivery re-folds
   and converges.

## Evidence

The prototype (branch `prototype/async-saga-state-model`) ran both readings over identical steps.

**Liveness.** Under event-as-data, the orchestrator records child reconciliation off the `transfer` topic but
only re-evaluates "are all children terminal?" off the `transaction` topic. A Transaction whose children both
completed can therefore sit in `started` forever. Fuzzing 2,000 runs × 400 steps: event-as-data left **13 runs
stalled and 55 short of a terminal state**; event-as-trigger left **none**, reaching terminal in 2,000 / 2,000.

This was initially mistaken for a cost of per-aggregate-type topics. It is not — a unified topic stalls too. It
is a property of the handler shape, and would have been recorded against the wrong cause.

**Idempotency.** The prototype's read-side dedup set was disabled and the happy path re-run with a duplicate
delivery after every publish. Terminal state and event count were **identical**. The domain reducer's own guards
(`reconcileChild` requires the child still be `requested`; re-evaluation requires the Transaction still be
`started`) already carry idempotency. A read-side table would add state that must itself be maintained, survive
consumer-group resets, and be reasoned about — for nothing. Hence corollary 1.

## Consequences

**The orchestrator stays coupled to the authoritative store.** It is not a self-contained stream consumer and
cannot be lifted out of the `go/` context or run anywhere without database access. This is the price of the
decision and should not be discovered later as a surprise.

**A lost message degrades to lateness, not wrongness.** A trigger that never arrives is a wake-up that never
happens, so the saga stalls until something else nudges it — but when a nudge does arrive it converges on truth
rather than replaying a hole. This is *why* root ADR 0001 must still guarantee publication completeness; trigger
semantics tolerates disorder and duplication, not loss.

**The unused proto triplets stay unused.** `proto/transfer/v1/transfer.proto` defines `Start*` / `*Started` /
`Complete*` event triplets that the sync saga skips, reserved "for a future async process manager". A
trigger-shaped process manager does not observe intermediate state through messages either, so they remain
unnecessary. Either leave them defined and unused deliberately, or remove them — but do not implement them on
the assumption this decision required them.

**Concurrent appends to a Transaction's stream become reachable.** With per-type topics, a transfer-topic
message and a transaction-topic message can be handled concurrently and both append to the same Transaction.
`appendSagaStep` already retries `ErrConcurrencyConflict`, so this is handled — but it is now a live path rather
than a theoretical one, and deserves a test.

## Open: rollback of a committed child

> **Resolved 2026-09-07 by [ADR 0002](0002-asynchronous-rollback-and-reversal-reconciliation.md).** The
> symmetric fix suggested below was built and measured: 3,000 runs × 500 steps, 0 violations, 0 stalls, all
> reaching a terminal state. The section is kept for the reasoning that led there.

**Not resolved by the prototype, and the largest remaining risk in the cutover.**

`rollbackChild` currently reverses a committed child by calling `RequestReversal` and then *synchronously*
reading `transfer.Outcome(reversalID)` to confirm the reversal actually committed. A Reversal is a new Transfer
aggregate running its own saga. Once that saga is asynchronous, the inline read is no longer meaningful: the
reversal's terminal event arrives later, through the transfer topic.

So `rollback_started` becomes a long-lived wait state with a second generation of children to reconcile, and
`TransactionRollbackFailed` — reachable today only through that inline check — needs a new route. The prototype
modelled rollback as instantaneous and so never exercised this; `rollback_failed` was unreachable in it.

**This must be designed before the cutover, and warrants its own ADR.** Under trigger semantics the shape is
probably natural — reversals reconcile exactly like forward children do — but "probably" is not a design.
