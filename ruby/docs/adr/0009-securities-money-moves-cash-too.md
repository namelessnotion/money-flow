# 9. Every Securities money leg has a cash leg, and a Security holds real cash

- **Status:** Accepted
- **Date:** 2026-09-23
- **Scope:** `ruby/` context only. Go is unchanged: it treats factory names and versions as opaque, and
  the widest shape here is three legs, well under
  [`go/docs/adr/0007`](../../../go/docs/adr/0007-bounded-transaction-width-and-sliced-dispatch.md)'s cap.
- **See also:** [ADR 0004](0004-ach-withdrawal-funds-before-it-leaves.md), whose withdrawal draws on both
  sides; [ADR 0006](0006-security-supply-is-ledger-enforced.md), whose decision 9 this corrects;
  [ADR 0007](0007-disbursement-is-one-transaction-per-holder.md).

## Context

An entity keeps money on two sides of the ledger. The **cash side** is `cash`: the real money the platform
holds for them. The **cleared side** is `uncleared_cash` and `cleared_cash`: how much of that money is past
the ACH return window and safe to spend. ACH keeps the two in step. A deposit credits both (a real leg
`bank → cash`, a shadow leg into uncleared cash, then Clearing). A withdrawal debits both: Funding takes it
out of cleared cash, and then the real leg takes it out of `cash`. `cash` permits neither direction, so its
Token carries `debits_must_not_exceed_credits`. That makes it the ledger's last guard against sending more
real money out over ACH than came in for that entity.

The Securities shapes as first built (ADR 0006) moved **only the cleared side**: a purchase's money leg,
the Draw, a Repayment and a Disbursement's payout. `cash` never moved. `bin/simulate_lending` reproduced the
result:

- A Borrower cannot withdraw a Draw. Funding succeeds, because the Draw credited cleared cash. The real leg
  is then refused: `wallet … has insufficient Token capacity`.
- An Investor cannot withdraw the interest they earned, because a Disbursement never credits their `cash`.
- An Investor who has invested still holds `cash` for money they have spent. Only Funding stops them
  withdrawing it.

CONTEXT.md said a Borrower gets drawn money to a bank with "an ordinary ACH withdrawal from the Borrower's
own cleared cash". That was false.

## Decision

1. **Money that moves between two parties inside the platform moves on both sides, by the same amount.**
   Every Securities **money leg** gets a **cash leg**, a second Transfer for the same amount between the
   same two parties, on the cash side. `Services::Securities::Leg.money` builds the pair, and no shape
   builds half of one.

2. **A Security holds real cash in a fourth Wallet, `security_cash`**, opened by `IssueOffering` alongside
   Supply, Escrow and Repayment, and `ALLOWS_NONE` like them. There is one of these per Security, not one per
   purpose wallet. Escrow and Repayment already keep escrowed money from paying a Disbursement; the cash side
   only has to say how much real money the Security holds. Its invariant: `security_cash` = Escrow +
   Repayment. `Services::Securities::Wallets.money_of_entity` and `.money_of_security` own the pairing:
   `cleared_cash` with `cash` for an entity, and a purpose wallet with `security_cash` for a Security.

   | Shape | Money leg | Cash leg |
   |---|---|---|
   | `security_purchase` | Investor `cleared_cash` → `security_escrow` | Investor `cash` → `security_cash` |
   | `security_draw` | `security_escrow` → Borrower `cleared_cash` | `security_cash` → Borrower `cash` |
   | `security_repayment` | Borrower `cleared_cash` → `security_repayment` | Borrower `cash` → `security_cash` |
   | `security_disbursement` | `security_repayment` → Investor `cleared_cash` | `security_cash` → Investor `cash` |

3. **A cash leg's id is derived from its money leg's:** `detid("<money leg id>:cash")`. No new columns. The
   rows still record exactly what was sent, because the cash leg's id is a function of an id they already
   hold. A re-send converges on Go's idempotency for both legs.

4. **A cash leg has the same parents as its money leg: a sibling, not a child.**
   - A purchase's money and cash legs both wait for its claim leg, so ADR 0006's oversubscription ordering
     is unchanged.
   - A Disbursement's payout and cash legs both wait for its retirement leg. When there is no retirement
     leg, both are roots.
   - The Draw's two legs and a Repayment's two legs are all roots, so Go's accept-time pre-flight checks
     all of them. The two legs in each pair draw on different Wallets, so there is no shared snapshot to
     over-accept against
     ([go ADR 0004](../../../go/docs/adr/0004-transaction-accept-time-funding-preflight.md)).

   Chaining the cash leg after its money leg would add one dispatch round trip to every Transaction. Since
   the async cutover that round trip is a CDC hop. It would buy nothing: with the two sides in step, the
   cash leg can fail only where its money leg would. When one sibling does fail, Go reverses the other
   ([go ADR 0002](../../../go/docs/adr/0002-asynchronous-rollback-and-reversal-reconciliation.md)).
   Neither leg is staged, so that rollback finishes in one call.

5. **Every shape that moves money is bumped to version `2`:** `security_purchase`, `security_draw`,
   `security_repayment`, `security_disbursement`. `security_offering` stays at `1`, because it moves
   claims, not money. The ACH factories are unchanged.

### Alternatives rejected

- **Change the withdrawal's real leg**, so it draws on something other than `cash`. The deposit's real leg
  would still credit `cash`, so `cash` would become a running total of everything ever deposited, which
  means nothing. Fixing that means redesigning deposits too and reopening ADR 0004 for both directions,
  over a problem that belongs to Securities. It also drops `cash`'s `debits_must_not_exceed_credits`, the
  ledger's own guard at the bank boundary.
- **Move `cash` directly between entities and bypass the Security**, for example Investors' `cash` →
  Borrower `cash` at the Draw. The Draw would then fan in across every holder, which is ADR 0007's fan-out
  inverted, with the same rollback and contention problems. Between purchase and Draw, the Investor's `cash`
  would still hold money they had spent.
- **Move only `cash` in Securities.** Cleared cash is what stops an entity spending a deposit the ACH
  network can still return. Securities have to keep checking it.
- **Credit Escrow and Repayment on both sides.** Each would read double its real balance, and every Balance,
  Stage or Money flow read of a Security would have to know to halve it.

## Consequences

- A Borrower can ACH-withdraw a Draw. An Investor can withdraw what a Disbursement paid them, interest
  included. An Investor who invests no longer holds `cash` for it. The CONTEXT.md claim is now true.
- An entity's `cash` never holds less than its cleared cash. Every movement either moves both by the same
  amount, credits `cash` first (a deposit), or debits cleared cash first (Funding). So a withdrawal whose
  Funding succeeds always has cash for its real leg.
- Each Securities Transaction runs one more Transfer: a purchase or Disbursement goes from 2 to 3, and a
  Draw or Repayment from 1 to 2. That is roughly 30–100% more Transfer sagas and events for Securities
  traffic, and the Postgres event store is the throughput ceiling. This is the price of the ledger being
  right. It is also a reason to keep cash legs as siblings rather than chaining them.
- A Security's four Wallets are not four piles of money. `security_cash` is the other side of Escrow +
  Repayment, so anything that sums "what a Security holds" must read one side only.
- **No backfill.** A Security issued before this change has no `security_cash`. Any v2 shape for it raises
  `MissingAccount` before Go is called. Entities whose two sides drifted under v1 stay drifted. For
  example, a Borrower drawn under v1 holds cleared cash with no `cash` behind it, so their v2 Repayment's
  cash leg is refused at pre-flight. This is development data only; reset it. If a real backfill is ever
  needed, it is one correcting cash-side Transaction per drifted party, plus provisioning `security_cash`
  for each open Security. Neither is built.
- ADR 0006 decision 9 still holds: Securities money never crosses the bank boundary. What it left out was
  moving the cash side inside the platform.
