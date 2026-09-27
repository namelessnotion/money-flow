# 11. A late return is recorded, and what the depositor can't cover is owed

- **Status:** Proposed
- **Date:** 2026-09-27
- **Scope:** `ruby/` context. Go is unchanged: it treats factory names as opaque and runs whatever DAG a shape
  sends. Addresses namelessnotion/money_flow#8, except its exposure policy, which is left to a follow-up.
- **See also:** [ADR 0003](0003-ach-clearing-as-a-scheduled-sweep.md), whose "what terminates the chain" this
  revisits; [ADR 0004](0004-ach-withdrawal-funds-before-it-leaves.md);
  [ADR 0010](0010-an-entitys-cash-covers-its-cleared-cash-in-flight.md);
  [go ADR 0016](../../../go/docs/adr/0016-a-leg-its-wallet-can-no-longer-cover-fails-at-prepare.md);
  `spec/alloy/ledger.als`.

## Context

A **Return** cancels the staged real leg. That works only until the entry settles. After that the leg is
committed, Go refuses `CancelStagedTransfer`, and nothing is recorded. Real returns don't stop at settlement.
Ordinary ones (R01 and the like) are due within 2 banking days of it, a withdrawal credit can come back once its
receiving account has closed (R02), and an unauthorized-debit return (R05, R07, R10) can arrive up to 60 days
later.

So a late deposit return is a loss the ledger never learns about, and first-party fraud exploits it: deposit,
wait for Clearing, withdraw the same money back, then dispute the deposit as unauthorized. A late withdrawal
return strands the customer's money outside the ledger until someone reconciles it by hand.
`spec/alloy/ledger.als` found that path as a counterexample to `LateReturnIsRecoverable`.

Go already has two ways to undo money, and neither fits:

- `RequestReversal` returns the exact Tokens the original moved. A deposit's are the `cash` Token its real leg
  minted, which the depositor may have spent, and a reversal is all or nothing.
- `StartTransactionRollback` refuses a Transaction that has completed.

## Decision

1. **A return notice is recorded before anything acts on it.** `ach_transactions` gains `returned_at`,
   `return_reason` and `return_transaction_id`. The notice is a fact the provider reported, like
   `provider_reference`, not lifecycle state. The first notice wins.

2. **A notice for a completed ACH Transaction is a late return, recorded as a Transaction of its own.** A notice
   for one still running cancels the staged real leg, as before. The late return takes one of three forms, each
   its own factory at version `1`, with ids derived from the ACH Transaction's
   (`detid("<ach id>:return")`):

   | Form | Factory | Legs, in order | Mints | Can the ledger refuse it? |
   |---|---|---|---|---|
   | Withdrawal return | `ach_withdrawal_return` | `bank → cash`, then `bank_control → cleared_cash` | both | no |
   | Clawback | `ach_deposit_clawback` | `uncleared_cash → bank_control`, then `cash → bank` | neither | no, while decision 3 holds |
   | Debt return | `ach_deposit_return` | `receivable → bank` | yes | no |

   Every form follows ADR 0010: an entity's cleared cash leaves before its cash, and its cash arrives before its
   cleared cash.

3. **A deposit is clawed back only if its Clearing was never recorded.** `uncleared_cash` is one pool of Tokens
   shared by every deposit an entity has not cleared, so a clawback of a deposit that did clear would silently
   take another deposit's money. The choice is made from Ruby's own record, under a lock on the ACH row.
   `Clear` records its ids before it calls Go, and takes the same lock. Once a notice is recorded, `Clear` refuses
   a deposit with no Clearing recorded, so the choice can never change afterwards:
   - no Clearing recorded: Go never received one, and never will. The deposit's money is still in uncleared
     cash, and ADR 0010 keeps `cash` covering it. It is clawed back whole.
   - a Clearing recorded: the money is, or will be, in cleared cash, where the depositor may spend it. The
     return is recorded as a debt (decision 4). The sweep keeps re-sending a recorded Clearing, so the money
     lands where Recovery reads it.

4. **What a depositor can't cover is owed, in a Receivable.** Every entity has a `receivable` account. A debt
   return mints the whole amount out of it, so the return is recorded in full whatever the depositor still holds,
   and the receivable's negative balance is what they owe. It is uncapped (`ALLOWS_ONRAMP_AND_OFFRAMP`), because
   minting needs onramp and Recovery credits it with fresh Tokens. It sits on neither side, so an entity's cash
   still equals its uncleared plus cleared cash at rest.

   Per entity, at rest, `bank − bank_control + receivable = 0`. Every dollar the network moved is either
   mirrored on the cleared side or owed. That is the ledger's own statement that every late return was
   recorded.

5. **Recovery collects what is owed from cleared cash.** A Recovery (`receivable_recovery` v1) moves
   `cleared_cash → bank_control`, then `cash → receivable`, for `min(owed, cleared cash not reserved)`. It is
   recorded in `receivable_recoveries` before Go is asked, with an id derived from the entity and a sequence
   number. A sweep originates one per entity at a time, and only once every earlier one has concluded. What is
   owed comes from Ruby's rows joined to the Transaction projection, never from token balances, which reach
   Ruby on another topic and can lag behind it. With the earlier Recoveries all concluded, what they recovered
   is known exactly, and debt never shrinks, so a Recovery can't take more than is owed. A Recovery the ledger
   refuses rolls back cleanly (go ADR 0016).

6. **An entity that owes can't send money where Recovery can't reach it.** While any debt is recorded and not
   recovered, a new ACH withdrawal and a new Subscription are refused. Both are read from Ruby's own rows, so
   the refusal starts with the notice, not when the projection catches up. Repayments, Draws and Disbursements
   are left alone: refusing a Repayment harms the Investors it pays, and the other two pay the entity, which
   gives Recovery something to collect.

## What terminates the chains

ADR 0003 said any follow-on that reacts to clearings must revisit its termination argument. Recovery is one.

- **Late returns** react to a return notice, and there is at most one per ACH Transaction, fixed by its derived
  id. Nothing reacts to a late return completing. The chain is one step long.
- **Recovery** reacts to what is owed and to cleared cash, which Clearings, Draws, Disbursements and withdrawal
  returns all change. Each Recovery that completes lowers what is owed by at least one minor unit, so there are
  finitely many. After one is refused or rolls back, the next is tried only once cleared cash has changed since,
  so a stale read can't make it retry forever. A Recovery whose rollback fails stops that entity's Recoveries:
  that needs a person.

## Alternatives rejected

- **Always record a debt, never claw back.** A returned deposit that hadn't cleared would sit in uncleared cash
  forever, out of Recovery's reach. Clearing it first would make returned money briefly withdrawable, and that is
  the most common return there is.
- **Let `cash` overdraw.** It drops `debits_must_not_exceed_credits` from `cash`, which every funded
  withdrawal's guarantee rests on (ADR 0004, ADR 0010).
- **Take what the depositor has, and write the rest off to a loss account.** Nothing on the platform is "the
  house" to hold that loss, and the money is still owed.
- **Decide the clawback from Go, and treat a Clearing Go has never heard of as never sent once some time has
  passed.** A slow `Clear` can still send after the wait. Only mutual exclusion is sound.

## Consequences

- A late return is visible in the ledger in both directions. A clawed-back deposit costs the platform nothing,
  and neither does a debt that is recovered.
- A debt that is never recovered is still a loss. It is now visible and bounded by the amount of the deposit,
  but nothing here stops the fraud in #8. That needs an exposure policy for unauthorized returns, which is left to
  a follow-up. `LateReturnIsRecoverable` in `spec/alloy/ledger.als` still fails, as it should until then.
- A withdrawal already in flight when a notice arrives is paid. That is exposure too, for the same follow-up.
- A recorded Clearing that Go refuses leaves the money in uncleared cash, where Recovery can't reach it. That
  needs a person.
- Entities onboarded before this change have no `receivable` account until a backfill opens one. Until then,
  their late deposit returns fail with `MissingAccount`, loudly and retryably.
