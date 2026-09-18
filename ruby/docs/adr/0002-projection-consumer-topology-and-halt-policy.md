# 2. One projection consumer, halting on what it cannot project

- **Status:** Accepted
- **Date:** 2026-09-18
- **Scope:** `ruby/` context only.
- **See also:** [ADR 0001](0001-read-model-consumer-and-follow-on-write-back.md), which left the consumer
  topology open. [`go/docs/adr/0003`](../../../go/docs/adr/0003-orchestrator-failure-handling.md), whose
  failure policy this adopts.

## Context

ADR 0001 decides *what* Ruby's consumer does: fold every published event into a read model, with a monotonic
guard, failing loudly on an unmapped event. It leaves open how the consumer is deployed, and what it does
when a message keeps failing.

## Decision

1. **One consumer group, `money-flow-ruby-projection`, reads both `transfer-events` and `transaction-events`**
   in one process (`ruby/bin/consumer`). Its offsets are independent of the Go orchestrator's
   `money-flow-saga-*` groups. A new group starts from the earliest offset, so it sees every event.
2. **It only projects.** No follow-on write-back runs in this consumer yet. When one is added, ADR 0001's
   warning still stands: first decide what terminates a chain of follow-on Transactions.
3. **An offset is committed only after its message is projected**, synchronously. Delivery is at-least-once,
   and the monotonic guard makes a repeated message a no-op.
4. **The failure policy is Go's (ADR 0003).** A transient failure is retried 3 times, with backoff doubling
   from 250ms. A message that still fails halts the consumer without committing it, and so does a message
   that can never succeed (malformed, or an unmapped event type) — at once. The process exits non-zero and
   is not restarted automatically: a restart would halt on the same message.
5. **It waits for its topics before subscribing**, by listing all topics. It never asks about a named topic,
   which could create it (the Go reader's e52ceec). Each check has its own timeout, and the wait gives up
   once no broker has answered for a minute.

## Consequences

- One bad message stops the whole read model, including the partitions it doesn't touch. That is the point:
  a skipped event is indistinguishable from lag, and the read model would stay wrong with nothing failing.
- The projection lags whenever the consumer is down. Callers see `null` state for an ACH Transaction the
  consumer has not reached yet (the GraphQL `AchTransaction` type says so).
- Splitting projection and write-back into separate groups remains possible later without a data migration:
  the projection's high-water marks live in its own rows, not in the group's offsets.
