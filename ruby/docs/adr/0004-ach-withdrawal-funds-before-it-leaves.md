# 4. An ACH withdrawal funds itself from cleared cash before any money leaves

- **Status:** Accepted
- **Date:** 2026-09-18
- **Scope:** `ruby/` context only. Go runs whatever transfer DAG it is sent and is unchanged.
- **See also:** [ADR 0003](0003-ach-clearing-as-a-scheduled-sweep.md);
  [go ADR 0002](../../../go/docs/adr/0002-asynchronous-rollback-and-reversal-reconciliation.md).

## Context

Withdrawal v1 ran the **real leg** (cash → bank, staged, over ACH) first and the **shadow leg** (cleared cash →
bank control) only once the real leg had posted. The balance check therefore happened *after* the money had
left: an entity whose deposits had not yet cleared was paid out over ACH, then its shadow leg was refused for
lack of cleared cash.

Go then rolled the Transaction back by reversing the committed real leg. A reversal inherits its Transfer's
staging (go ADR 0002), so that reversal was staged: it waited for an external confirmation to claw the money
back from the customer's bank. Nothing in Ruby sends one, so those Transactions sat in `rollback_started`
indefinitely. Two did in development (`ACH E2E`, `ItsAlive`).

## Decision

1. **A withdrawal's real leg depends on its shadow leg** (`ach_withdrawal` v2, `TransactionShape`). Cleared
   cash moves to bank control first; that step is called **funding**. A shortfall refuses the shadow leg, and
   Go rolls the Transaction back with nothing yet moved across the bank boundary. Deposits keep v1: their
   shadow leg still waits for the real leg, so uncleared cash is only minted for money that arrived.
2. **`Initiate` asks Go how the Transaction stands before submitting to the provider.** Go runs non-staged legs
   synchronously inside `StartInitializingTransaction`, so one `ResumeTransaction` afterwards is definitive:
   `STARTED` means funded with the real leg staged; anything else is refused with Go's reason and the entry is
   never submitted.
3. **A rolled-back withdrawal undoes its funding with an ordinary reversal.** The shadow leg is not staged, so
   neither is its reversal: after a return, the rollback completes in the same call.

## Consequences

- A withdrawal an entity cannot cover fails at `initiateAchWithdrawal` with a GraphQL error, and its record
  shows `FUNDING` failed and `ROLLBACK` done.
- Cleared cash is held in bank control for as long as the ACH entry is outstanding, so it cannot fund a second
  withdrawal in the meantime.
- A staged reversal can still arise, from reversing a *deposit's* committed real leg, which only happens if a
  deposit's shadow leg fails after settlement. Ruby still has no path that confirms a staged reversal; that
  remains open.
- Withdrawals recorded under v1 keep their history. Their `steps` read against the v2 lifecycle, so a v1
  withdrawal can show `FUNDING` waiting beside a completed submission.
- The two v1 withdrawals stuck in development are not repaired by this change.
