# 2. Rollback is a wait state, and a Reversal reconciles like any other child

- **Status:** Accepted
- **Date:** 2026-09-07
- **Scope:** `go/` context only.
- **Resolves:** the open question recorded in
  [ADR 0001](0001-event-triggered-saga-orchestrator.md#open-rollback-of-a-committed-child).

## Context

`rollbackChild` undoes a committed child by calling `RequestReversal` and then *synchronously* reading
`transfer.Outcome(reversalID)` to confirm the reversal actually committed. A Reversal is a new Transfer
aggregate running its own saga, so once that saga is asynchronous the inline read returns `accepted`, not
`committed`. It can no longer confirm anything.

That is not merely a testing gap. Inspecting `childState` shows the concept is missing outright:

```go
childUntouched, childGated, childRequested, childCompleted,
childFailed, childRolledBack, childRollbackFailed
```

The forward path has `childRequested` — "dispatched, waiting, resolve me later from live outcome" — and
`reconcileInFlight` resolves it. **The rollback path has no equivalent.** `foldChildStates` confirms it: the
only rollback events are the two terminal ones. Nothing can represent "reversal R is in flight for child X".

`TransactionRollbackFailed` is also reachable today *only* through that inline check, so removing it removes the
state's only route.

## Decision

1. **Add `childRollbackRequested`**, recorded by a new event
   **`TransferReversalRequestedWithinTransaction { id, transfer_id, reversal_id }`**. It carries the
   deterministic reversal id, so redelivery cannot request a second reversal.
2. **Resolve it exactly as the forward path resolves `childRequested`**: on any trigger, read the Reversal's
   live `transfer.Outcome`. Committed → `TransferRolledBackWithinTransaction(REVERSED)`. Failed or cancelled →
   `TransferRollbackFailedWithinTransaction`. Anything else → still waiting, no change.
3. **`rollback_started` becomes a long-lived wait state.** A Transaction may sit in it for days. Only the
   `committed` branch waits; abandonment and cancellation still resolve in one step, because
   `CancelStagedTransfer` and `CancelAcceptedTransfer` are synchronous RPCs that complete before returning.
4. **`TransactionRollbackFailed` is reached only by a child reaching `childRollbackFailed`**, once every child
   is resolved.

## Evidence

Prototype branch `prototype/async-saga-state-model`, extended with Reversals as real child aggregates and a
three-child DAG (`C` depends on `A` and `B`). Under the configuration adopted in the root ADR — CDC, per-type
topics, event-as-trigger, keyed by aggregate id — **3,000 runs × 500 steps: 0 violations, 0 stalls, and all
3,000 reached a terminal Transaction state** (2,572 `rolled_back`, 425 `rollback_failed`, 3 `completed`). 477
runs created at least one Reversal; 137 of those created a *staged* Reversal.

Three invariants were added for this and held throughout: a child is only `rolled_back` once its Reversal
actually committed; rollback never undoes a child while something that depended on it is unresolved; and
rollback terminates.

The symmetric design was a hypothesis before this ran. It is now measured.

## Consequences

**A rollback can wait on the outside world, because a Reversal inherits staging.** `rollbackChild` passes
`Stage: spec.GetStage()` straight through to `RequestReversal`, so reversing a staged Transfer creates a *staged
Reversal*, which parks at `staged`/`pending` until an external confirmation arrives. Rollback of an
ACH-settled transaction therefore takes as long as an ACH settlement.

**Do not alert on "`rollback_started` for more than N hours".** It would fire on correct behaviour. The
prototype's stall check had to learn the same distinction — it now separates *waiting on a non-terminal child or
reversal* from *drained and not progressing*, and only the latter is a defect. Operational alerting needs that
same split.

**Rollback is multi-sweep and DAG-ordered.** `readyToRollback` must skip children already in
`childRollbackRequested` — they are in flight, not ready — or a redelivery will re-request their reversal. The
deterministic id makes that harmless, but the sweep should not depend on that.

**A second generation of aggregates now exists inside one Transaction.** Reversals appear on `transfer-events`
like any other Transfer, are keyed by their own `aggregate_id`, and are consumed by the same orchestrator. No
new topic, no new consumer.

## Note on the prototype's third child

Validating this needed a three-child DAG with a join, not a two-child chain. With `B` depending on `A`, the only
reversible child is `A` — `B` completing implies `A` completed, so nothing fails and no rollback occurs. The
staging-required child could never be the one reversed, and the staged-Reversal path above was **unreachable and
would have been missed**. Worth remembering when writing tests: a linear chain will not exercise this.
