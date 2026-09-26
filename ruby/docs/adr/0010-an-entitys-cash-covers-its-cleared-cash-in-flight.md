# 10. An entity's cash covers its cleared cash, even in flight

- **Status:** Accepted
- **Date:** 2026-09-26
- **Scope:** `ruby/` context. Go is unchanged: it runs whatever DAG a shape sends. Resolves
  namelessnotion/money_flow#7.
- **Supersedes:** [ADR 0009](0009-securities-money-moves-cash-too.md) decision 4 (a cash leg is its money
  leg's sibling).
- **See also:** [ADR 0004](0004-ach-withdrawal-funds-before-it-leaves.md), `spec/alloy/ledger.als`
  (`FundedWithdrawalCanBePaid`), `go/internal/transaction/two_sides_race_test.go`.

## Context

A withdrawal's Funding checks only cleared cash. Its real leg then draws on `cash`, and a withdrawal that has
been funded must be paid (ADR 0004, and *Cash side and cleared side* in CONTEXT.md). That holds only if an
entity's `cash` never holds less than its cleared cash. It has to hold at every moment, not just once every
Transaction has finished.

ADR 0009 made every movement inside the platform move both sides, and ran the money leg and its cash leg side
by side. It argued that chaining them "would buy nothing: with the two sides in step, the cash leg can fail only
where its money leg would". That is true for one Transaction on its own. It isn't true for two in flight
together. `spec/alloy/ledger.als` found a four-step counterexample:

1. A Borrower holds 1 of cleared cash and 1 of cash. A Repayment of 1 and a withdrawal of 1 both start, and
   each passes Go's pre-flight on its own.
2. The withdrawal's Funding takes the cleared cash.
3. The Repayment's cash leg takes the cash.
4. The withdrawal is funded, but its real leg is refused: there is no cash left.

Each Transaction took one side of the same dollar. No money is lost, since both roll back, but a funded
withdrawal wasn't paid. Purchases, Draws and Disbursements have the same shape.

## Decision

1. **A money leg and its cash leg run in order, never side by side.** The order covers the entity:
   - when the entity pays (a Subscription, a Repayment), its **cleared cash leaves first**, then its cash;
   - when the entity is paid (a Draw, a Disbursement), its **cash arrives first**, then its cleared cash.

   `Leg.entity_pays` and `Leg.entity_is_paid` name the order, and `Leg.chain` turns it into dependencies:

   | Shape | Order |
   |---|---|
   | `security_purchase` | claim → money → cash |
   | `security_repayment` | money → cash |
   | `security_draw` | cash → money |
   | `security_disbursement` | retirement → cash → payout (cash → payout when interest-only) |

   ACH already follows the rule. A deposit's real leg credits `cash` before its shadow leg credits uncleared
   cash, and a withdrawal's Funding debits cleared cash before its real leg debits `cash`.

2. **With that, an entity's cash covers its cleared cash (uncleared and cleared together) at every moment.**
   Every debit of `cash` follows the same Transaction's debit of cleared cash, and every credit of cleared cash
   follows the same Transaction's credit of `cash`. So whichever Transaction takes an entity's cleared cash
   first has the cash behind it, and any other is refused before it touches the cash. Rollback reverses legs
   children-first (go ADR 0002), which keeps to the same rule.

3. **Every shape that moves money goes to version `3`.** `security_offering` stays at `1`.

## Why the entity and not the Security

Each pair of legs moves both parties at once, so no order covers both. Money-first leaves the payee briefly
with cleared cash that has no cash behind it, and cash-first does the same to the payer. Every Securities shape
runs between an entity and a Security, and only an entity can withdraw, so the order covers the entity.

A Security's `security_cash` can therefore trail its Escrow plus Repayment while a Subscription or Repayment is
in flight. What that can cost is a Draw or Disbursement landing in the gap, finding `security_cash` short, and
rolling back. Neither overlaps its predecessors in practice: a Draw follows a fully subscribed offering, and a
Disbursement starts only once its Repayment has completed. Since go ADR 0016, a roll back is clean either way.

## Alternatives rejected

- **Correct the promise instead.** Keep the legs as siblings, and say a funded withdrawal can be paid only while
  nothing else of the entity's is in flight. It's the cheapest change, but people would see withdrawals fail
  that they were told had been funded.
- **Allow one money-moving Transaction in flight per entity.** That removes the race entirely, but it
  serializes an Investor's Subscriptions and auto-invest, and needs a queue and a lock.
- **Move both sides with one Transfer.** A Transfer runs from one source wallet to one destination wallet. Two
  wallet pairs need two Transfers, or a new Go concept of linked Transfers.

## Consequences

- A withdrawal that is funded can be paid, whatever else the entity has in flight. `FundedWithdrawalCanBePaid`
  in `spec/alloy/ledger.als` changes from `expect 1` to `expect 0`. It's proven for one Repayment beside one
  withdrawal: up to 2 Transactions, 4 Transfers, 6 Accounts and 6 states. The version 2 shapes fail at that
  same scope, so the scope is big enough to hold the bug. For Draws, `DrawRollbackNeverFails` is proven too:
  with the cash leg first, reversing a Draw can't be refused.
- Each Securities Transaction takes one more dispatch hop, a CDC round trip: this is the cost ADR 0009 declined
  to pay.
- Go now pre-flights only the first leg of each pair. A Repayment is still refused up front when the Borrower's
  cleared cash is short, and their cash can't be short when their cleared cash isn't. A Draw is pre-flighted on
  `security_cash` rather than on escrow. Before any Repayment the two are equal, so a Draw sent twice is still
  refused up front.
- Transactions started under version 2 finish under version 2. Only new ones are ordered.
