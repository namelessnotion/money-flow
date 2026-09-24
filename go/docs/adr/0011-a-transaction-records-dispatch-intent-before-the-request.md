# 11. A Transaction records its intent to request a child before it makes the request

- **Status:** Accepted
- **Date:** 2026-09-24
- **Scope:** `go/` context. No event is added, removed or renamed. `TransferRequestedWithinTransaction`
  keeps its name but now means something slightly earlier (see Consequences). Ruby maps it to no state
  (`event_state_map.rb`), so nothing it projects changes.
- **See also:** [ADR 0001](0001-event-triggered-saga-orchestrator.md), which lets a transfer-topic run and a
  transaction-topic run fold the same Transaction at once. [ADR 0005](0005-saga-dispatch-claim-prevents-double-invocation.md),
  whose "The Transaction layer needs no claim of its own" is amended here. [ADR 0007](0007-bounded-transaction-width-and-sliced-dispatch.md)
  defines the slice this records in one append.
- **Amended 2026-09-24 by** [ADR 0014](0014-a-transaction-starts-with-its-first-slice.md): `TransactionStarted` now
  leads the first slice's intents in their one append, as the last paragraph under Consequences proposed.

## Context

`requestChildTransfer` called `RequestTransfer` and only afterwards appended `TransferRequestedWithinTransaction`
through `appendSagaStep`. That append deduplicated by `(type, transfer_id)` and checked nothing else, so it
landed whatever the Transaction had become in the meantime. A rollback running between the call and the
append could conclude without ever seeing the child:

1. Run X (transaction topic) folds a Started Transaction with ready roots A and B. It requests A, then records
   it.
2. X calls `RequestTransfer(B)`. B's Transfer accepts on its own stream.
3. Run Y (transfer topic, woken because A failed) resumes the Transaction. It records A failed, starts
   rollback and abandons A. B is untouched in its fold, so `readyToRollback` skips it, and Y appends
   `TransactionRolledBack`.
4. X appends `TransferRequestedWithinTransaction(B)` after the terminal. `runSaga` returns at once on
   RolledBack, so nothing ever reverses B. B's own saga commits it.

That leaves money moved by a committed Transfer under a RolledBack Transaction. An operator's
`StartTransactionRollback` landing mid-slice has the same shape, and so does `StartProcessingTransfer`, which
read a child as Gated and recorded the request after making it. `TestRunSaga_DoesNotStartATransactionThatWasRolledBackWhileStarting`
only guarded Initialized → Started.

The same unconditional append allowed two smaller versions:

- a forward child outcome, read while Started, landing after the rollback had resolved that child, which
  rewrote a rolled-back child as completed;
- `StartTransactionRollback`, which read Started, landing after `TransactionCompleted` and reopening the
  Transaction.

`dispatch_race_test.go` reproduces each of these against the real `transfer.Server`. It holds one run inside
`RequestTransfer` for B, after B accepts, until a concurrent rollback has concluded. Before this change B
committed under a RolledBack Transaction in every variant.

## Decision

**1. Intent comes before effect.** `dispatchReady` appends the whole slice's
`TransferRequestedWithinTransaction` / `TransferGatedWithinTransaction` in one append against the version of
the fold that chose it (`recordIntents`), and only then calls `RequestTransfer`. There are two cases:

- A rollback that lands first makes the append lose. The run re-folds, sees the rollback, and requests
  nothing.
- A rollback that lands after the intents finds B Requested, so the rollback must undo it.

`recordIntents` neither dedupes nor retries, because an intent licenses a side effect and may only be recorded
against the exact fold that decided it. `StartProcessingTransfer` records its one intent the same way, against
the fold that found the child Gated. It also refuses once the Transaction is no longer Started.

**2. Accept or reject is observed afterwards, through `transfer.Outcome`.** An accept records nothing: the
Transfer's own events wake the Transaction (`saga.Orchestrator.handleTransfer`), and `reconcileInFlight`
reads them as before. A rejection is still recorded by the call that got it, as
`TransferFailedWithinTransaction` with the Transfer's reason, because a rejected Transfer names no Transaction
(`transfer.OwningTransaction`) and wakes none. `reconcileInFlight` now also treats a Requested child whose
Transfer is NotFound or Rejected as an interrupted dispatch. It completes the request with the same idempotent
`RequestTransfer` call, which records a rejection.

**3. Rollback finishes an intent it finds unfinished.** `rollbackChild` on a Requested child whose Transfer is
NotFound completes the idempotent `RequestTransfer` itself, then undoes the child by what it became. Usually
that is `CancelAcceptedTransfer`. The dispatching run and the rollback reach the same single decision for the
child, whichever calls first. A rejected child is ABANDONED. If the child's own saga moves past cancelling
first, `CancelAcceptedTransfer`'s FailedPrecondition makes `rollbackChild` re-read the outcome and undo the
child by that instead (cancel if staged, reverse if committed).

**4. `appendSagaStep` drops a step whose fold has been overtaken** (`stillDecidable`):

| Step | Recorded only while |
|---|---|
| `TransferCompleted/FailedWithinTransaction` | Started, and the child is still Requested |
| `TransactionRollbackStarted` | Initialized or Started |
| `TransferReversalRequested/RolledBack/RollbackFailedWithinTransaction` | RollbackStarted |
| `TransactionCompleted/RolledBack/RollbackFailed` | `conclusion()` still decides exactly that |

Any other event is an error, because intents do not go through `appendSagaStep`. Every caller already re-folds
after appending, so a dropped step is decided again from whatever overtook it. `StartTransactionRollback`
reports FailedPrecondition when its rollback was dropped because the Transaction completed first.

## Consequences

**Nothing can be recorded after a Transaction's terminal event**, and no child can be requested that its
rollback will not see. `dispatch_race_test.go` pins both, including a dispatch interrupted between intent
and request, recovered going forward (`TestResume_RequestsAChildWhoseIntentOutlivedItsDriver`) and going back
(`TestRollback_ResolvesAChildWhoseIntentOutlivedItsDriver`).

**`TransferRequestedWithinTransaction` now means "decided to request", not "requested and accepted".** For a
moment, or until the next run if the driver dies, a Requested child may have no Transfer stream. Readers that
assumed one exists have changed:

- `ChildTransferIDs` (and so `cmd/resume`) lists only children with a stream.
- `rollbackChild` and `reconcileInFlight` handle NotFound.

A rollback may therefore complete a child's request only to cancel it at once. That is one wasted Transfer, and
only in the race window or after a crashed driver.

**A slice costs one commit for its intents instead of one per child.** Two runs that fold the same stream can
no longer both dispatch from it: the loser's append conflicts and it re-folds. Before, both called
`RequestTransfer` for the same children and relied on idempotency (ADR 0005). ADR 0005's "no claim needed"
argument still holds for the calls themselves. What it missed is that the Transaction's record of a call must
be ordered against its own state, not only deduplicated.

**`TransactionStarted` is no longer load-bearing as a claim.** Its compare-and-swap append stopped a rollback
recorded during Initialized from being overwritten by a start. The first slice's intents are now CAS'd against
the same fold, so they would catch that race on their own. It is kept as the published fact that the
Transaction started. Folding it into the first slice's append would save one more commit per Transaction, and
is left for a later change.

**`StartProcessingTransfer` can now answer Aborted** when three appends in a row lose to other writes on a
busy Transaction, the same retryable answer the other RPCs give. A duplicate call after one succeeded is
refused as "already processed", as a sequential retry already was.
