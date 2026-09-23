# 4. An ACH withdrawal funds itself from cleared cash before any money leaves

- **Status:** Accepted
- **Date:** 2026-09-18
- **Scope:** `ruby/` context primarily — the DAG shape (decision 1) and the request/response contract Ruby relies
  on (decision 2) are decided here. [go/docs/adr/0004](../../../go/docs/adr/0004-transaction-accept-time-funding-preflight.md)
  (2026-09-19) later changed *how* Go arrives at that contract for the common case; it does not change what
  Ruby depends on, and is scoped there rather than amended into this one.
- **See also:** [ADR 0003](0003-ach-clearing-as-a-scheduled-sweep.md);
  [go ADR 0002](../../../go/docs/adr/0002-asynchronous-rollback-and-reversal-reconciliation.md);
  [go ADR 0004](../../../go/docs/adr/0004-transaction-accept-time-funding-preflight.md).

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
2. **Something asks Go how the Transaction stands before submitting to the provider.** `start_transaction`
   itself now catches the common shortfall — [go/docs/adr/0004](../../../go/docs/adr/0004-transaction-accept-time-funding-preflight.md)
   has Go reject an unfundable withdrawal in its own accept/reject decision, before writing anything — but
   `Initiate` still calls `ResumeTransaction` once afterward regardless, and that call remains the one thing
   that is actually definitive: `STARTED` means funded with the real leg staged; anything else is refused with
   Go's reason and the entry is never submitted. Both checks exist for the same guarantee; the second is what
   makes it hold even in the residual case where the first passed but the real, unchanged dispatch that follows
   it disagrees moments later.

   > **Amended 2026-09-22: the check moved, the guarantee did not.** `Initiate` could make it because the whole
   > DAG ran inside its call. Since
   > [`go/docs/adr/0006`](../../../go/docs/adr/0006-synchronous-dispatch-removed-from-the-rpc-surface.md) Go
   > accepts the Transaction and returns, so at that point the funding leg has not run and asking would only ever
   > answer `INITIALIZED`.
   >
   > `Services::Ach::SubmitDue` makes it now, and the pairing is **stronger** than this decision shipped with:
   > the real leg must be `staged` in the projection — which for a withdrawal is unreachable except past the
   > dependency edge on the funding leg, so it *proves* funding rather than inferring it from "started and not
   > terminal" — and only then is Go asked, immediately before the entry is handed over.
   > [ADR 0008](0008-ach-submission-is-a-sweep.md) has the whole of it.
3. **A rolled-back withdrawal undoes its funding with an ordinary reversal.** The shadow leg is not staged, so
   neither is its reversal: after a return, the rollback completes in the same call.

## Consequences

- A withdrawal an entity cannot cover fails at `initiateAchWithdrawal` with a GraphQL error. **(Still true
  after the async cutover: the funding leg is ready at time zero, so Go's pre-flight sees it and rejects before
  writing anything. This is the case customers actually hit, and it is still answered immediately.)** Since
  [go/docs/adr/0004](../../../go/docs/adr/0004-transaction-accept-time-funding-preflight.md), the common case
  never starts at all: Go rejects the Transaction outright, and its record shows `INITIATION` failed with
  everything after it — including `FUNDING` — `SKIPPED`, and no rollback step (nothing was ever started to roll
  back). The original shape this decision shipped with — `FUNDING` failed, `ROLLBACK` done — is still reachable
  for the residual race decision 2's second check exists to catch, where Go's own pre-check passed but
  the real dispatch that follows it disagrees moments later. **Since the async cutover that race is the normal
  asynchronous path rather than a rarity — the dispatch happens in the orchestrator, after the response — so a
  withdrawal that fails this way is learned from the projection rather than from an error.**
- Cleared cash is held in bank control for as long as the ACH entry is outstanding, so it cannot fund a second
  withdrawal in the meantime.
- A staged reversal can still arise, from reversing a *deposit's* committed real leg, which only happens if a
  deposit's shadow leg fails after settlement. Ruby still has no path that confirms a staged reversal; that
  remains open.
- Withdrawals recorded under v1 keep their history. Their `steps` read against the v2 lifecycle, so a v1
  withdrawal can show `FUNDING` waiting beside a completed submission.
- The two v1 withdrawals stuck in development are not repaired by this change.
