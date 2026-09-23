# 8. An ACH entry is submitted by a sweep, once the ledger has actually moved

- **Status:** Accepted
- **Date:** 2026-09-22
- **Scope:** `ruby/` context only.
- **See also:** [ADR 0004](0004-ach-withdrawal-funds-before-it-leaves.md), whose decision 2 this
  relocates and whose guarantee it keeps; [ADR 0003](0003-ach-clearing-as-a-scheduled-sweep.md) and
  [ADR 0007](0007-disbursement-is-one-transaction-per-holder.md), the two sweeps this transliterates;
  [ADR 0001](0001-read-model-consumer-and-follow-on-write-back.md) decision 4, which requires a
  follow-on chain's terminator be established before shipping;
  [`go/docs/adr/0006`](../../../go/docs/adr/0006-synchronous-dispatch-removed-from-the-rpc-surface.md),
  which forced this.

## Context

ADR 0004 decision 2 is a money-safety rule: an ACH entry must not reach the provider unless the
withdrawal behind it is funded. `Services::Ach::Initiate` enforced it by asking Go, straight after
running the Transaction, whether it was still `STARTED` — *"the only thing that ever learns the real
dispatch's actual outcome"*.

That check worked because the whole DAG ran inside the call. Since `go/docs/adr/0006` it does not:
Go accepts the Transaction and returns. At the end of `Initiate#call` the funding leg has not been
dispatched, let alone committed. Asking anyway would get back `initialized`, every time, and
submitting on that would be paying out a withdrawal nobody had funded.

**This is worth stating plainly, because an earlier draft of this change got it wrong.** The obvious
repair — keep the check, move it a little later, wait for `STARTED` — is unsafe. `runSaga` appends
`TransactionStarted` and only then dispatches, and dispatch merely *accepts* the funding leg. Under
the cutover `STARTED` is reachable before any money has moved at all.

## Decision

1. **Submission moves out of `Initiate` into `Services::Ach::SubmitDue`, a scheduled sweep.**
   `Initiate` records intent and asks Go to accept the Transaction. That is all it does.

2. **An entry is submitted only when all three hold**, checked in this order:

   ```
   1. ach_transactions.provider_reference IS NULL              — not already submitted
   2. transfer_projections[real_transfer_id].state = 'staged'  — proves funding completed
   3. GetTransactionState(id).state == STARTED, immediately before submitting
   ```

3. **(2) is a read-model read, and that is sound.** The projection may lag Go but it cannot lead it:
   every state it holds is one Go actually reached, under the monotonic guard of ADR 0001 decision 2.
   For a **withdrawal** the real leg sits behind the dependency edge on the funding leg, so `staged`
   is unreachable unless funding committed — it *proves* funding rather than inferring it from the
   Transaction being started and not yet terminal, which is all the old single check could do. For a
   **deposit** the real leg is the DAG root and staging is its own first step, so the same predicate
   reads correctly for both directions with no branch.

4. **(3) is the definitive check, and it is ADR 0004 decision 2, relocated.** (2) establishes a past
   fact; only a live authoritative read rules out something having gone wrong since. The residual
   window between (3) and the provider call is unchanged from before, and ADR 0004 decision 3 already
   covers it: the funding is undone by an ordinary, unstaged reversal.

5. **`provider_reference` is the terminator, and it is Ruby's guard rather than Go's.** It is written
   as soon as the provider gives it and before Go is told anything, because an entry the provider
   holds must always be traceable; it is also what takes the row out of the candidate set for good.
   This departs from ADR 0001 decision 4, which puts idempotency in Go — deliberately, because the
   effect is *outside the ledger* and Go cannot dedupe it. Resubmission safety within the crash
   window between the provider call and the write is the `Provider` port's contract; the entry
   carries the Transaction id as its idempotency key.

6. **Every minute, every day.** Cron's floor, and unlike clearing nothing here waits on a calendar.
   This interval is the whole of the delay between a customer asking for a withdrawal and the entry
   reaching the provider, so it should be as short as the scheduler allows. Weekends included: an
   entry should queue for the next banking day rather than sit out the weekend. `submitAchNow`
   exists for demonstrations, mirroring `clearAch` — and unlike `clearAch` it skips nothing, because
   the sweep's only reason for waiting is the schedule, and a shortcut here would be a way to pay out
   an unfunded withdrawal.

## What terminates the chain

ADR 0001 decision 4 requires this before shipping, and it is the third follow-on chain.

Submission reacts to a real leg reaching `staged` on a row with no `provider_reference`. Submitting
sets that column, and the row never becomes a candidate again. `confirm_staged` moves the leg to
`pending`, and **nothing reacts to `pending`**: `Settle` and `Return` are the provider reporting what
became of the entry, not reactions to anything Go published. The chain is one step long, bounded at
one submission per ACH Transaction.

## Consequences

- **The guarantee is stronger than it was.** The old single check could only establish that the
  Transaction was running; the new pair establishes that funding actually happened and that nothing
  has undone it since.
- **A withdrawal reaches the provider up to a minute after it is asked for.** It was immediate.
- **Which refusals a caller still sees synchronously**, and which now arrive through the projection:

  | Still synchronous, from `initiateAchWithdrawal` | Now a projected state |
  | --- | --- |
  | Non-positive amount, unknown entity (Ruby's own) | The residual ADR 0004 race: pre-flight passed, the real dispatch disagreed → `rolled_back` |
  | Go's `TransactionRejected`: malformed DAG, over-wide Transaction, zero-amount leg | Provider refusal at submission → `Return` → `rolled_back` |
  | **The common underfunded withdrawal**, caught by Go's accept-time pre-flight, because the funding leg is ready at time zero | Anything failing after funding |

  So the narrowing is genuinely narrow: the case a customer actually hits still fails immediately,
  with Go's own reason.
- **A `confirm_staged` that fails after a successful submit is an operator situation**, and the sweep
  records it as such: the provider holds an entry the ledger says was cancelled. `provider_reference`
  is already persisted, so it is traceable. It lands in `Result.failed`, and so in Resque's failed
  queue.
- **A rolled-back or rejected Transaction never becomes a candidate**, and is never retried. It needs
  a person — the same posture ADR 0003 and ADR 0007 take.
- **`Services::Ach::Progress` needed no change.** It already derives `Submission` from
  `provider_reference` and already models every step as possibly `waiting`.

## Why a sweep rather than reacting to `TransferStaged` in the consumer

Considered and rejected, for the third time in this context's history and for the same reasons ADR
0003 and ADR 0007 give. A sweep catches up on its own after any outage; nothing per-entry lives only
in Redis; and it does not widen what a bad message in `ruby/bin/consumer` can take down — that
consumer only projects, which ADR 0002 decision 2 decided deliberately and which reacting here would
reopen.

If a minute ever proves too slow, that is the change to make, and it needs ADR 0002 amended rather
than worked around.
