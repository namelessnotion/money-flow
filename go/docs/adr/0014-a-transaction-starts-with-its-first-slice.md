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

**It saves a commit, not throughput, on the local stack.** Measured 2026-09-24 on the local Docker Desktop
stack. Each run was `cmd/simulate -entities 150 -transactions 3000 -concurrency 48` with the ledger check on. The
comparison covers ADRs 0011–0014 together against ADR 0010 (`263ad33`). `go` and `orchestrator` were recreated
before every block, and the blocks alternated old, new, old, new so that drift across the sitting would show:

| Block | Code | transaction mode | transfer mode (control) |
|---|---|---|---|
| A1 | ADR 0010 | 260.8, 261.4 /s | 357.6 /s |
| B1 | ADR 0014 | 262.3, 261.5 /s | 365.7 /s |
| A2 | ADR 0010 | 263.0, 264.5 /s | 393.0 /s |
| B2 | ADR 0014 | 261.2, 270.4 /s | 396.7 /s |

- **Transaction mode:** ADR 0010 averaged 262.4/s and this change 263.9/s. The +0.6% is inside the spread
  between runs of the same code.
- **WAL flushes (`wal_sync`) per transaction-mode run:** ~33.6k before and ~33.0k after.
- **The transfer control drifted up through the sitting on both sides**, so compare only within one block pair.
  An earlier comparison that restarted only the new side read as +9%. That gain came from the restart.
- **The commit saving itself is exact.** Counting distinct `xmin` per Transaction stream across the runs gave
  4.00 → 3.00 commits for a completed Transaction and 5.00 → 4.00 for a rolled-back one, with Started sharing
  its first slice's commit in 100% of streams.

At concurrency 48, group commit already shares one WAL flush among concurrent commits, so one fewer commit per
Transaction is not one fewer flush. The saving should matter more where flushes are not shared: lower
concurrency, or a disk with slower fsync than this one.
