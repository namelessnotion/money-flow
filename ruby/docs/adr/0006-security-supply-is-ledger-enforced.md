# 6. A Security's Supply is a wallet, and its claim leg gates its money leg

- **Status:** Accepted
- **Date:** 2026-09-20
- **Scope:** `ruby/` context only.
- **See also:** [ADR 0004](0004-ach-withdrawal-funds-before-it-leaves.md), whose funding-first ordering this
  mirrors; [go ADR 0004](../../../go/docs/adr/0004-transaction-accept-time-funding-preflight.md), whose
  accept-time pre-flight it relies on and whose limits it works around; [ADR 0007](0007-disbursement-is-one-transaction-per-holder.md).

## Context

An Issuer offers a Security in a Borrower's debt obligation, and Investors buy fractions of it. The offering
must not be oversubscribed: the sum of every purchase can never exceed the offering size, including when
several Investors buy at once.

The obvious shape is a Ruby-side check — sum the Subscriptions, compare against the offering size, refuse if
short. It is also the wrong one. Ruby's view of the Subscriptions is a lagging fold of published events
(ADR 0001), so two concurrent purchases can each read a figure that was true a moment ago and both be
allowed. The check would be advisory while reading as authoritative, which is the worst of both.

Go's ledger already enforces exactly this kind of constraint. A Wallet whose `Allows` permits neither
direction gives each of its Tokens TigerBeetle's `debits_must_not_exceed_credits`, so that account can never
be drawn below what was credited into it. What was missing was a reason for a Security to have a wallet at
all.

## Decision

1. **An entity has exactly one role** — `investor`, `borrower` or `issuer` — and the role decides which
   accounts onboarding opens for it. Every role gets what an ACH shape moves money through; beyond that a
   role gets only what its own part in the market uses. An account an entity can never move money through is
   a Wallet nobody will be able to explain later.

2. **A Security's Supply is a wallet, not a number.** Each Security gets three of its own — `security_supply`,
   `security_escrow`, `security_repayment` — opened on its *Issuer's* Holder, because Go has no Holder for a
   Security. They hang off `accounts` with a `security_id`, under a biconditional CHECK, rather than in a
   table of their own: `accounts.wallet_uuid UNIQUE` is the invariant that matters and it has to hold across
   both kinds, which two tables could not give us.

3. **The Supply is minted once, for exactly the offering size**, from the Issuer's `issuer_control` wallet
   with `mint_source: true`. `issuer_control` is `ALLOWS_ONRAMP_AND_OFFRAMP`, and not as a matter of taste:
   Go's `validateMintSource` refuses `mint_source` from anything narrower, and minting is the only way claims
   enter the ledger. Its Token therefore carries no flag and may run permanently negative — which is right,
   because that negative *is* total claims outstanding, and it returns to zero as claims are retired back
   into it. The same shape as `bank_control`.

4. **Oversubscription is a ledger refusal, not a Ruby check.** `security_supply` is `ALLOWS_NONE`, so its
   Token cannot be debited past what was minted. Ruby's "remaining" figure is a read-model number for the UI
   and for a friendly early error; it is never an authority, and it may let through a purchase the ledger
   then refuses.

5. **A purchase's claim leg is the DAG root and its money leg depends on it.** `security_supply` is the hot
   wallet — every concurrent purchase draws on it — and it is exactly where go ADR 0004's known limitation
   bites: several children ready at time zero from the same wallet are each checked against one unconsumed
   snapshot, so the pre-flight can over-accept. Ordering the claim leg first means that in the common case an
   oversubscription is a clean `TransactionRejected` before Go writes anything, and when the race does slip
   past, the claim leg fails at real dispatch with the money leg never having run: nothing to reverse, and no
   Investor money moved. Had the money leg been a root, every lost race would reverse a committed payment.

6. **The money leg is deliberately not pre-flighted, and we accept what that costs.** `wouldAcceptReadyChildren`
   only walks children ready at time zero, so a child behind a dependency edge is never checked. An Investor
   short of cleared cash therefore gets a Transaction that initializes and then rolls back, with Go reversing
   the already-committed claim leg (go ADR 0002). `Services::Securities::Purchase` calls `ResumeTransaction`
   and reports that as a refusal, because the response to *starting* the Transaction cannot be the last word
   for a gated leg. Its cleared-cash pre-check is advisory only.

7. **A Disbursement mirrors the same ordering** for the same reason: the retirement leg is the root, so the
   `investment` wallet's `debits_must_not_exceed_credits` refuses an over-payment of principal before money
   leaves the repayment wallet.

8. **Never build a zero-amount leg.** Go turns `minor_units == 0` into a Twirp error that the Transaction
   saga logs and swallows: the RPC still answers `TransactionInitialized`, and the Transaction sits in
   `Started` for ever with no event to explain it. A refusal would be survivable; that is not. An
   interest-only Repayment therefore has no retirement leg at all rather than a zero one, and three CHECK
   constraints say so where they can be relied on.

9. **Securities money never crosses the bank boundary.** The Borrower receives a Draw into cleared cash and
   repays from cleared cash; reaching a real bank is the existing ACH withdrawal and ACH deposit-plus-clearing.
   Nothing here stages, so a Transaction runs to completion inside the call that starts it.

10. **The Draw is operator-initiated**, not a reaction to the last Subscription completing. See ADR 0007 for
    what that buys.

## Consequences

- An underfunded Investor produces a rolled-back Transaction that is visible in GraphQL and needs no operator
  action — but it is not the clean pre-accept refusal an ACH withdrawal gets, and the error arrives a moment
  later.
- Concurrent purchases on the same Security contend on one wallet. A lost race is a `Refused` the client can
  retry, which is correct behaviour for a sold-out offering anyway.
- Onboarding's account set changed for every role: `debit_card`, `gain` and `loss` are no longer given to
  everyone. Existing entities predate roles and were backfilled to `investor`.
- A Security with unsold Supply after its offering window needs a policy this ADR does not set.
- The zero-amount stall is a latent trap for any future shape, not just this one. Ruby works around it in
  three places; Go should reject it in `validateDAG` at accept time.

  > **Done, 2026-09-22.** `validateDAG` now refuses any leg `money.Validate` would refuse — zero minor units,
  > a missing currency, or no amount at all — as one `TransactionRejected` written before anything else exists
  > ([`go/docs/adr/0007`](../../../go/docs/adr/0007-bounded-transaction-width-and-sliced-dispatch.md)). Ruby's
  > three workarounds stay as belt-and-braces rather than being removed: they give a better message at the call
  > site, and the CHECK constraints stop a bad row being written at all. The stall this describes mattered more
  > after the async cutover, not less — with nothing watching the RPC response, a swallowed transport error
  > reached nobody at all.
