# 14. A Transaction starts in the same commit as its first slice

- **Status:** Accepted
- **Date:** 2026-09-24
- **Scope:** `go/` context. No event is added, removed or renamed, and the published language is unchanged.
  `TransactionStarted` is still written, once per Transaction and still second on its stream. It now shares its
  commit with the first slice's `TransferRequestedWithinTransaction` intents. Ruby sees the same events in the
  same per-stream order.
- **See also:** [ADR 0011](0011-a-transaction-records-dispatch-intent-before-the-request.md), whose Consequences
  left this for a later change. [ADR 0010](0010-facts-decided-together-share-a-commit.md), the rule this applies.

## Context

On its first fold of an Initialized Transaction, the orchestrator appended `TransactionStarted` on its own, then
re-folded and appended the first slice's intents. That is two commits for one decision: the fold that finds the
Transaction Initialized is also the fold that chooses its first slice, and nothing happens between the two.

Started's own append was a compare-and-swap against the Initialized fold. Its only job was to lose to a
`StartTransactionRollback` recorded while Initialized. Since ADR 0011, the first slice's intents make the same
check against the same fold, so they would catch that race on their own.

A single-child Transaction that completed cost 4 commits on its own stream: Initialized, Started, the intent, and
the child's completion with `TransactionCompleted`.

## Decision

**Starting a Transaction is dispatching its first slice.** When `runSaga` folds a Transaction as Initialized, it
calls `dispatchReady` with `starting` set. `TransactionStarted` leads the slice's intents in the one append
`recordIntents` makes against that fold, so the start lands or loses together with the requests it licenses:

- A rollback recorded first makes the append lose. The run re-folds, finds RollbackStarted, and requests nothing.
  `TestRunSaga_DoesNotStartATransactionThatWasRolledBackWhileStarting` still pins this.
- A concurrent run that started the Transaction first makes the append lose too. The run re-folds as Started and
  takes whatever is still ready.

If the first slice leaves children behind, the run ends as any sliced run does, and the slice's own appends fetch
the next one. `TransactionStarted` is written even if nothing is ready, so a start is never skipped. `validateDAG`
guarantees at least one root, so this does not happen in practice.

## Consequences

**Every Transaction costs one commit fewer.** A single-child Transaction that completes costs 3 commits on its
stream instead of 4, and a failing one with nothing to undo costs 3 instead of 4 (ADR 0013's budget, re-pinned).
`TestRunSaga_StartsInTheSameAppendAsTheFirstSlice` pins the shape: `[TransactionStarted,
TransferRequestedWithinTransaction × one slice]` in one append, and exactly one Started per stream.

**Initialized still lasts until the orchestrator folds it**, so a Transaction sitting in Initialized still means
publication or the orchestrator is behind. What goes away is the brief Started state with nothing dispatched.

Not measured under `cmd/simulate` yet.
