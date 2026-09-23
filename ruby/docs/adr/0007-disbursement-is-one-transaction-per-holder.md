# 7. A Disbursement is one Transaction per holder, originated by a derived-id sweep

- **Status:** Accepted
- **Date:** 2026-09-20
- **Scope:** `ruby/` context only.
- **See also:** [ADR 0001](0001-read-model-consumer-and-follow-on-write-back.md) decision 4 and its warning on
  follow-on chains; [ADR 0003](0003-ach-clearing-as-a-scheduled-sweep.md), whose sweep this transliterates;
  [ADR 0006](0006-security-supply-is-ledger-enforced.md).

## Context

When a Borrower repays a Security, every holder is owed a share of it: their principal back, and the interest
earned on it. A Security may have many holders.

The shape that first suggests itself is one Transaction fanning out to all of them — it is, after all, one
event in the business. Go's Transaction DAG can express it. It should not.

This is also Ruby's second follow-on chain, so ADR 0001 requires deciding what terminates it before shipping.

## Decision

1. **One Go Transaction per (Repayment, holder), never one fan-out.** Three reasons, each from Go's own code:

   - `dispatchReady` runs a Transaction's ready children **serially and in-process, inside the synchronous
     RPC that starts it**. A two-hundred-holder fan-out is two hundred Transfer sagas — each with its own
     event-store load, TigerBeetle write and append — in one HTTP call. That is a timeout, and worse, a
     *retried* timeout: `TwirpCall` would re-enter a call Go is still executing.
   - Every payout leg draws on the same `security_repayment` wallet, which is exactly the case where
     `wouldAcceptReadyChildren` evaluates siblings against one unconsumed snapshot and can over-accept
     (go ADR 0004).
   - The Transaction is the unit of rollback. One holder's leg failing would roll back the whole thing,
     reversing every holder already paid — via one reversal Transfer each, running their own sagas. One bad
     holder would undo everyone.

   Per-holder Transactions invert all three: each is two legs, one holder's failure is one holder's failure,
   and nothing contends because the sweep sends them one at a time.

   > **Amended 2026-09-22. The first reason is dissolved; the other two survive and the decision stands.**
   >
   > [`go/docs/adr/0006`](../../../go/docs/adr/0006-synchronous-dispatch-removed-from-the-rpc-surface.md)
   > removed synchronous dispatch from the RPC surface. `dispatchReady` no longer runs a Transfer saga at
   > all — it accepts each child and the orchestrator runs it — so "two hundred Transfer sagas in one HTTP
   > call… a *retried* timeout re-entering a call Go is still executing" is simply no longer true. That was
   > the loudest of the three and it is gone.
   >
   > The second is **worse** than when this was written: the gap between
   > `wouldAcceptReadyChildren` and the real dispatch was microseconds and is now a CDC round trip, so an
   > over-accept against one unconsumed snapshot is more reachable, not less.
   >
   > The third is untouched. The Transaction is still the unit of rollback, and
   > `TransactionRollbackFailed` is still a terminal that requires a person.
   >
   > Go now enforces a width limit of its own
   > ([`go/docs/adr/0007`](../../../go/docs/adr/0007-bounded-transaction-width-and-sliced-dispatch.md)),
   > so a two-hundred-holder fan-out is refused at accept time rather than merely avoided by this
   > decision's discipline. The reasoning there is the same two reasons, which is why this decision
   > survives its own first argument being withdrawn.

2. **Its ids are derived** — `detid("<repayment id>:disbursement:<investor id>")`, and `…:payout` /
   `…:retirement` for the legs. Go's idempotency on `StartInitializingTransaction` is the only duplicate
   guard, as ADR 0001 prescribes. The `disbursements` row is a record of what was sent, not an "already
   disbursed" flag.

3. **A recurring sweep, not a delayed job per Repayment.** resque-scheduler runs `Jobs::DisburseRepayments`
   every five minutes, every day — unlike ACH clearing nothing here waits on a calendar, and a Borrower may
   repay on a weekend. Nothing per-Repayment lives only in Redis, so a lost Redis or a dead worker is caught
   up by the next run. A (Repayment, holder) pair the read model has seen in **any** state is left alone: in
   flight it needs nothing, and rejected or rolled back needs a person, not a retry loop. One sent but not
   yet seen is sent again, which Go dedupes. One holder failing does not stop the rest; the run then fails,
   so it lands in Resque's failed queue.

4. **The allocation is pro rata by outstanding principal, principal and interest split separately**, rounded
   by largest remainder with ties to the lower entity id.

   Separately is load-bearing. Allocating the combined total and carving it back into the two would let a
   holder's *principal* share come out above their outstanding principal on a rounding boundary — and their
   retirement leg would then be refused by the `investment` wallet's `debits_must_not_exceed_credits`,
   turning a rounding choice into a Disbursement stuck for ever.

   So is the determinism. The Transaction id is derived while the amount is **not**, so a sweep that re-runs
   must compute the same amount it computed the first time; otherwise Go's idempotency returns the
   Transaction that moved the first amount while Ruby records the second, and the two disagree for ever.
   `ProRata` therefore compares remainders as exact integers rather than floats, and does not depend on the
   order its inputs arrive in — which is why `Positions` orders by entity id rather than leaving it to
   callers, where forgetting it would be silent.

5. **The invariants are asserted, not merely tested.** `ProRata.allocate` checks that its parts sum to
   exactly the total; `Allocation.for` checks each bucket separately and that no holder's principal share
   exceeds what they hold. A split that quietly loses a minor unit is the failure these exist to prevent, and
   it would otherwise surface at a ledger reconciliation months later rather than at the call site.

6. **A holder's claim is retired before they are paid**, and the retirement leg is the DAG root — see
   ADR 0006 decision 7.

7. **Never build a zero-amount leg** — ADR 0006 decision 8. An interest-only Repayment retires nothing, so
   its Disbursement has no retirement leg and the payout leg becomes the root. Holders allocated nothing at
   all are dropped from the allocation rather than sent an empty Transaction.

## What terminates the chain

A **Repayment completing** triggers Disbursements, and **nothing reacts to a Disbursement completing**. The
chain is exactly one step long by construction.

It is one step and not two because the **Draw is operator-initiated** rather than a reaction to the last
Subscription completing (ADR 0006 decision 10). Had the Draw been a sweep too, this capability would have
introduced two chains at once. Any future follow-on that reacts to a Disbursement must revisit this.

The upper bound on work is holders × Repayments, fixed and finite: a pair is proposed until the read model
has seen a Transaction for its derived id, and never again after that.

## Consequences

- A holder is paid within five minutes of their Repayment being seen, not instantly. Nothing here promises
  otherwise, and `disburseRepaymentNow` exists for demonstrations the way `clearAch` does.

  > **Amended 2026-09-22.** "Seen" now costs more than it did. `due` requires the Repayment's Transaction to
  > read `completed` in the projection, and since the async cutover reaching `completed` takes several CDC
  > round trips rather than happening inside the call that started it. The five minutes is still the sweep's
  > own; what precedes it is longer.
- A Disbursement that rolls back needs a person. It is visible in GraphQL as a state on the row, and the
  sweep will not touch it again.
- An allocation that will not add up fails its own Repayment and no others; the sweep logs it and carries on.
- A Repayment against a Security with no holders raises rather than quietly disbursing nothing — the
  Borrower's money would otherwise sit in the repayment wallet with nobody owed it. It is unreachable
  through the services, since a Repayment needs a drawn Security and a Draw needs a fully subscribed one.
