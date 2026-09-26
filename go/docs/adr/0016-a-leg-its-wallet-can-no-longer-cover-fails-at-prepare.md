# 16. A leg its wallet can no longer cover fails at prepare

- **Status:** Accepted
- **Date:** 2026-09-26
- **Scope:** `go/` context. `TransferFailed` gains a route from `Accepted`. Its fields don't change, and the
  proto only changes in comments. Resolves namelessnotion/money_flow#6.
- **See also:** [ADR 0003](0003-orchestrator-failure-handling.md), [ADR 0004](0004-transaction-accept-time-funding-preflight.md),
  `spec/alloy/ledger.als`.

## Context

A Transfer selects its source Tokens twice. `RequestTransfer` checks the wallet can cover the amount when it
accepts, but reserves nothing. `prepare()` selects again once the orchestrator gets to it, one CDC hop later. If a
concurrent Transaction debits the same wallet in between, the second selection comes up short.

`tryPrepare` answered that with a plain error. The error is permanent, because nothing refills the wallet on its
own. The Transfer stayed `Accepted`, its Transaction never concluded, and under ADR 0003 the orchestrator retried
three times and then halted every saga. Two Transactions from one entity that debit the same wallet are enough to
hit this, for example a Borrower repaying while withdrawing. Replaying a counterexample from `spec/alloy/ledger.als`
found it. The model had treated accept and prepare as one step.

## Decision

1. **A shortfall at prepare is a domain outcome.** `tryPrepare` records `TransferFailed`, with the shortfall as
   its reason, rather than returning an error. `Accepted -> Failed` joins the Transfer's transitions. Nothing has
   reached TigerBeetle, so there is nothing to compensate. The owning Transaction treats the failure like any other
   failed child and rolls back.
2. **`TransferFailed` is written only on the stream as prepare loaded it.** `failUnprepared` appends at the
   loaded length. If anything landed first, prepare re-plans from the new state, as for any overtaken plan. It
   doesn't go through `recordOutcome`, because that re-folds on a conflict. Since `Prepared -> Failed` is also
   allowed, a re-fold could fail a Transfer that a concurrent prepare had already moved on, with its commit
   claimed.

`TransferFailed` still means "the ledger couldn't cover this Transfer, and it will never move money". A second way
of reaching it doesn't change that meaning, so the read model's mapping stands: `ruby/app/consumer/event_state_map.rb`
maps it to `failed` without looking at prior state.

## Consequences

- A Transaction whose leg runs short between accept and prepare rolls back, and the orchestrator keeps going. The
  race itself remains: accept still reserves nothing. What changes is how the race ends.
- The read model can now see a Transfer fail without ever being prepared. It has no legs and records no
  `TokenBalanceRecorded`.
- `tryPrepare` still returns errors for the mint_source re-validation, a destination mint rejection, and a
  Reversal's manifest failing after accept. These would halt the orchestrator the same way, but no scenario is
  known to reach them. None of them depends on a balance another Transaction can spend: a Reversal's manifest
  reads only the original's recorded legs. A Reversal whose destination has since been spent is refused by
  TigerBeetle at commit, and is compensated as before.
