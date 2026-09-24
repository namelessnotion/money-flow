# 10. Facts decided together share a commit

- **Status:** Accepted
- **Date:** 2026-09-24
- **Scope:** `go/` context. No event is added, removed or renamed, and the published language is unchanged.
  [ruby ADR 0005](../../../ruby/docs/adr/0005-account-balances-are-projected-from-token-balances.md) decision 3
  is amended: balances now arrive *with* the step's outcome rather than just before it.
- **See also:** [ADR 0009](0009-operations-are-transfer-legs-not-aggregates.md), the previous round of this work,
  which removed events. [ADR 0005](0005-saga-dispatch-claim-prevents-double-invocation.md) defines the claims,
  one of which now moves into `prepare()`.

## Context

Throughput is set by how many Postgres commits a Transfer costs, since each one waits on a WAL flush
(ADR 0009's Context). After ADR 0009, nothing left in the log is redundant. Every event is a decision, a claim
guarding a TigerBeetle call, or a balance the read side projects. But several facts that are always decided
together were still written in separate commits.

A staged, committed Transfer, the shape `cmd/simulate` runs, cost **11 commits**:

| Step | Commits |
|---|---|
| accept | 1 |
| prepare | 1 |
| stage: claim, then TigerBeetle, then one balance per touched Token, then `TransferStaged` | 1 + 2 + 1 |
| confirm (`TransferPending`, Ruby's call) | 1 |
| commit: claim, then TigerBeetle, then one balance per touched Token, then `TransferCommitted` | 1 + 2 + 1 |

`TokenBalanceRecorded` was the largest event type in the log, about 3.9 per Transfer, each committed on its
own. A Transaction also recorded its last child's outcome and its own conclusion in separate appends.

The same measurement turned up a bug. 8 of 12,600 Transfers carried a second `CancellingStagedTransferStarted`
after `TransferCancelled`. A duplicate `CancelStagedTransfer` read Staged in the handler, lost the race to the
first cancel, then reloaded inside `cancelStaged()` and claimed from Cancelled, taking whatever state it found
as its pre-claim state. It re-voided in TigerBeetle (answered `Exists`) and left a claim on a finished Transfer.
`commit()` had the same shape.

## Decision

**1. A step's outcome and the balances its ledger write moved land in one atomic write.**
`stage()`, `commit()` and `cancelStaged()` end in `recordOutcome`. It builds each touched Token's
`TokenBalanceRecorded` (`token.BalanceWrites`) and appends them with the outcome in one `AppendAtomic`.
`submitBatch` no longer records balances when every chain is accepted. The one exception is a refusal after
earlier chains applied: no outcome carries those, so they are still recorded one append per Token.

- The ordering Ruby relies on is unchanged. Each Token's stream is loaded before its account is read, and each
  write expects the loaded length, so the last balance on a stream was read after every earlier one.
- Losing the append is contention: a shared source Token moved, or a `*Rejected` response landed on the
  Transfer's stream. The whole write is rebuilt, balances re-read, and retried while it keeps losing to
  something (the same rule as `prepare()`, ADR 0003's amendment). A conflict nothing caused is still a fault.
- `AppendAtomic`'s contract is widened to name this use: recording a step's outcome with the observations of
  the ledger write it made. An observation decides nothing about the Token. The "not a licence to mutate
  independently-lived aggregates" rule stands for decisions.

**2. `prepare()` claims the next step in the same write as `TransferPrepared`.** The marker
(`StagingTransferStarted`, or `TransferCommittingStarted` for an immediate Transfer) follows
`TransferPrepared` in the Transfer's write, and `prepare()` hands the claim to `runSaga`, which goes straight
into `stageClaimed`/`commitClaimed`. A driver that dies in between leaves a claim, not an unclaimed Prepared
Transfer. Once stale, the same step takes it over (ADR 0005 Round 4), as with any abandoned claim.

**3. A child's outcome that decides the Transaction's conclusion is appended with it.** `conclusion(events)`
is now the single place that decides a Transaction's ending:
- Completed: every child resolved and none failed.
- RolledBack or RollbackFailed: nothing left to roll back and nothing still being reversed.

`runSaga` asks it where it used to decide inline. `appendSagaStep` asks it after a child's outcome, and when
that outcome concludes the Transaction, appends both events in one append. Rollback's next move is read off the
fold by `planRollback`, shared by `rollbackNext` and `conclusion` so they cannot disagree. An RPC's own decision
(`StartTransactionRollback`) is still recorded alone, and the orchestrator concludes from it (ADR 0006).

**4. A step claims only from the states it is legal in.** `cancelStaged()` from Staged or Pending, `commit()`
from Prepared or Pending. A caller that arrives after the Transfer moved on returns nil, and the RPC handler
re-reads the outcome.

## Consequences

**A staged, committed Transfer costs 6 commits instead of 11**, and an immediate one 3 instead of 6. Both
budgets are pinned in `leg_lifecycle_test.go`. A Transaction saves one more commit when it concludes.

Measured 2026-09-24 on the local Docker Desktop stack. Each run was `cmd/simulate -entities 150
-transactions 3000 -concurrency 48` with the ledger check on, twice per side. The baseline was main
immediately before:

| | before | after |
|---|---|---|
| transfer mode | 319.9, 324.6 /s | 369.4, 380.4 /s (**+16%**) |
| transaction mode | 226.7, 229.6 /s | 267.7, 258.3 /s (**+15%**) |
| WAL flushes, transfer-mode run | 30.7k, 30.2k | 19.4k, 21.2k (−33%) |
| WAL flushes, transaction-mode run | 36.6k, 39.5k | 28.4k, 30.9k (−22%) |

Events written per run were unchanged (transfer ≈ 20.7k, token ≈ 15.5k).

**No event moved streams, and none was added or removed.** Ruby sees the same events in the same per-stream
order. Only the grouping into commits changed. What Ruby *can* now rely on is stronger: a Token's balance and
the step that moved it become visible together.

**A cancel on a Prepared Transfer now always loses.** Before, it could win the few milliseconds between
`TransferPrepared` and the next step's claim. Now that claim lands with `TransferPrepared`, so
`CancelAcceptedTransfer` waits the step out and finds the Transfer staged or committed. A cancel on an
Accepted Transfer is unaffected.

**A hot Token retries whole outcomes, not single balances.** Previously a lost balance append retried only that
Token. Now any touched Token's conflict rebuilds the step's whole write. Each retry is still bounded by
`contention.Wait` and only happens when something else landed.

**Tests that drive `stage()`/`commit()` by hand seed an unclaimed Prepared Transfer** (`prepareUnclaimed`,
which strips claim markers from `prepare()`'s write). Such a stream is what an older build wrote, and it
stays a state the saga has to handle.
