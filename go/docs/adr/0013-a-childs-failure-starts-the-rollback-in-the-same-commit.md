# 13. A child's failure starts the rollback in the same commit, and a failed child needs no rollback record

- **Status:** Accepted
- **Date:** 2026-09-24
- **Scope:** `go/` context. No event is added, removed or renamed. `TransferRolledBackWithinTransaction` is no
  longer written for a child already recorded as `TransferFailedWithinTransaction`, and `ROLLBACK_METHOD_ABANDONED`
  now means only "the rollback found this child failed on its own". Ruby maps both per-child events to no state
  (`event_state_map.rb`), so nothing it projects changes.
- **See also:** [ADR 0010](0010-facts-decided-together-share-a-commit.md), whose decision 3 this extends from a
  Transaction's conclusion to the start of its rollback. [ADR 0011](0011-a-transaction-records-dispatch-intent-before-the-request.md),
  whose `stillDecidable` table this leaves unchanged.

## Context

Once the rest of the Transaction had nothing left to undo, a child's failure still cost three commits:

| Commit | Events |
|---|---|
| the failure, from `requestChildTransfer` or `reconcileInFlight` | `TransferFailedWithinTransaction` |
| `runSaga`, on its next fold | `TransactionRollbackStarted` |
| `rollbackNext`, then `conclusion` | `TransferRolledBackWithinTransaction{ABANDONED}`, `TransactionRolledBack` |

Neither of the later commits waited on anything:

- **`TransactionRollbackStarted` follows from the failure alone.** Every child failure rolls the whole Transaction
  back. Nothing outside the stream has to answer first, which is the test ADR 0010 set for sharing a commit.
- **The `ABANDONED` record for that child restated its failure.** `rollbackChild` wrote it for any child recorded as
  Failed without reading anything. A failed child was rejected, failed or cancelled, so it moved no money, and its
  failure is already on the stream. The record existed only so `planRollback` would count the child as resolved.

A Transaction with a committed sibling paid the same extra commits: 5 from the failure to `TransactionRolledBack`
where 3 are enough.

In the dev event log, about 21.6k of the 110k rollbacks begin with a `TransferFailedWithinTransaction`. The rest were
started by `StartTransactionRollback` and are unaffected.

## Decision

**1. A child recorded as Failed counts as resolved in a rollback.** `planRollback` counts `childFailed` the way it
counts `childRolledBack`. `readyToRollback` therefore neither offers a failed child nor lets one block its parents,
which it could not do anyway, having moved nothing. `rollbackChild` is only ever offered a Requested or Completed
child now, and treats anything else as an error. `ABANDONED` is still written when the rollback reads a Requested
child's live outcome and finds it rejected, failed or cancelled. That child's failure was never recorded, so the
rollback record is the only fact about it.

**2. `consequence` decides every step that follows from the stream alone.** `conclusion` is renamed `consequence`,
and it now also returns `TransactionRollbackStarted` while the Transaction is Started and some child has failed. The
reason names the failed child, as `runSaga` used to. `runSaga` asks `consequence` instead of deciding the rollback
inline.

**3. `appendSagaStep` chains consequences into the child's append.** After a child's outcome, it keeps asking
`consequence` of the stream as it would read with everything pending, until the answer is nil. The chain is at
most two steps: `RollbackStarted` then `RolledBack` or `RollbackFailed`, or `Completed` alone. An RPC's own decision
(`StartTransactionRollback`) is still recorded alone (ADR 0006). The first event is still checked by
`stillDecidable`, and the chained ones are decided from that same fold.

## Consequences

**A failed child with nothing else to undo costs 1 commit instead of 3.**
`TestTransaction_AChildsFailureAndTheRollbackItStartsShareOneAppend` pins it: `[TransferFailedWithinTransaction,
TransactionRollbackStarted, TransactionRolledBack]` in one append, and 4 appends for the whole Transaction. **A
failure beside a committed sibling costs 3 instead of 5**: the failure and the rollback start share one append, and
the Reversal is requested and resolved as before.

**A failed child's last record is its failure.** It folds to `childFailed` for good, where it used to end as
`childRolledBack`. `IsOpen`, `ChildTransferIDs` and `TerminalEventTypes` do not look at per-child state, and Ruby
ignores it. The rollback slicing tests seed children whose Transfers were rejected but not yet recorded, since a
recorded failure no longer costs the rollback an append.

**The rollback starts even if later children of the same slice are still being requested.** Nothing changes for
them. `dispatchReady` recorded their intents first (ADR 0011), so the rollback finds them Requested and undoes them.
Before, it would have started from another run's fold, a moment later.

Not measured under `cmd/simulate` yet. In the dev log, most rollbacks were started by `StartTransactionRollback`,
which this does not change, so a throughput gain there would be small.
