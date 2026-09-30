module moneyflow/dag

/*
 * Can every Transaction that go/ accepts actually complete?
 *
 * go/ accepts any DAG of Transfers (proto/transaction/v1/transaction.proto,
 * `transfers` and `transfer_dependency`) that validateDAG and the funding
 * pre-flight let through (go/internal/transaction/dag.go). This model looks
 * for one that can never complete: however the legs are ordered, one of them
 * can't be covered when it runs, so the Transaction is bound to roll back.
 *
 * ledger.als can't ask this, because it pins every Transfer to one of Ruby's
 * shapes. Here, legs are arbitrary: any wallets, any amounts, any DAG.
 *
 * Dependency structure alone never dooms a Transaction. validateDAG refuses a
 * cycle, so every DAG it accepts has a topological order. Only the money can
 * doom one.
 *
 * Sources for each fact are cited inline. When one of them changes, this file
 * must change with it.
 */

---------------------------------------------------------------------------
-- 1. WALLETS
---------------------------------------------------------------------------

-- A Wallet's Allows policy (proto/shared/v1). Every Token minted into the
-- Wallet gets the TigerBeetle flags it maps to (go/internal/token/allows.go),
-- destination Tokens included (token.MintWrites).
abstract sig Allows {}
one sig OnrampAndOfframp, Onramp, Offramp, NoAllows extends Allows {}

-- A Wallet collapsed to what one of its legs can spend: the sum of its
-- Tokens' positive balances. selectSourceTokens (go/internal/transfer/
-- manifest.go) skips any Token at or below zero, whatever the Wallet allows,
-- so an uncapped Wallet can't be overdrawn by an ordinary leg either. Only a
-- mint_source leg drives a Token negative, and that Token is never spent.
sig Wallet {
  allows:    one Allows,
  spendable: one Int
}

fact WalletsHoldWhatTheyCan {
  all w: Wallet {
    w.spendable >= 0 and w.spendable <= 3  -- keeps Int arithmetic in range
    -- An ONRAMP Token carries credits_must_not_exceed_debits, so it never
    -- holds a positive balance.
    w.allows = Onramp implies w.spendable = 0
  }
}

---------------------------------------------------------------------------
-- 2. ONE TRANSACTION'S LEGS
---------------------------------------------------------------------------

-- A Transfer inside the Transaction (transaction.proto, message Transfer).
-- Every Leg in an instance belongs to the one Transaction under question.
-- Other Transactions are left out, so "doomed" means doomed on the opening
-- balances, the same stance the pre-flight takes (go ADR 0004). Beside this
-- one, another Transaction can take balance away (go ADR 0016). Once it
-- completes, it can also pay in: its Tokens become visible to selection
-- (wallet.TokensOfVisibleTo), and that can rescue a doomed Transaction. The
-- first two classes below are doomed whatever the balances.
sig Leg {
  src:     one Wallet,
  dst:     one Wallet,
  amt:     one Int,
  parents: set Leg   -- transfer_dependency: legs that must complete first
}

-- mint_source legs: prepare() mints a fresh source Token instead of selecting
-- existing ones (transfer.mintSourceLeg).
sig Minting in Leg {}

-- `stage` isn't modelled. This model asks whether some execution completes,
-- and in the best one the network settles a staged leg at once. A pending
-- debit only holds balance back, a pending credit can't be spent, and
-- children wait for the leg to post in either case. So a staged leg behaves
-- like a posted one here.

-- What validateDAG checks: positive amounts and no cycle. It doesn't check
-- that from_wallet_id and to_wallet_id differ, and nor does RequestTransfer,
-- so a leg may move money within one Wallet.
fact WellFormedDag {
  all t: Leg {
    t.amt > 0 and t.amt <= 3
    t not in t.^parents
  }
}

fun ancestors[t: Leg]: set Leg { t.^parents }
fun descendants[t: Leg]: set Leg { t.^~parents }

---------------------------------------------------------------------------
-- 3. WHAT A LEG NEEDS WHEN IT RUNS
---------------------------------------------------------------------------

-- A Wallet's spendable balance once the legs in `done` have completed. It
-- depends on which legs have completed, not on their order. The whole model
-- rests on that.
fun balance[w: Wallet, done: set Leg]: Int {
  minus[plus[w.spendable, (sum l: done & dst.w | l.amt)],
        (sum l: (done - Minting) & src.w | l.amt)]
}

-- Whether leg t, once its parents have completed, would complete given
-- `done`:
-- - an ordinary leg must find its amount in the source Wallet's Tokens, when
--   RequestTransfer accepts it and again when prepare selects (go ADR 0016);
-- - a mint_source leg needs a source Wallet that allows onramp
--   (validateMintSource), and nothing else from its source;
-- - its destination Token is minted with the destination Wallet's flags, so
--   an ONRAMP destination refuses the credit (credits_must_not_exceed_debits
--   on a Token that has no debits).
-- A child is accepted any time after its parents complete: the saga reacts to
-- events, and nothing bounds how late. So the accept check can always fall
-- just before the leg runs, and is the same check.
pred runnable[t: Leg, done: set Leg] {
  t not in done
  t.parents in done
  t in Minting implies t.src.allows in Onramp + OnrampAndOfframp
  t not in Minting implies balance[t.src, done] >= t.amt
  t.dst.allows != Onramp
}

-- What go/ checks before it writes TransactionInitialized, beyond
-- WellFormedDag. wouldAcceptReadyChildren checks each root that isn't
-- mint_source, one at a time, against its Wallet's balance right now.
pred Preflight {
  all t: Leg - Minting | no t.parents implies t.src.spendable >= t.amt
}

---------------------------------------------------------------------------
-- 4. A CERTIFICATE THAT NO ORDER COMPLETES
---------------------------------------------------------------------------

-- "No order of the legs completes" quantifies over orders, which Alloy
-- can't do directly. But because a balance depends only on the set of legs
-- that completed, the Transaction can complete iff the set of all legs is
-- reachable from the empty set, one runnable leg at a time. It is
-- unreachable iff some family of leg sets contains the empty set, is closed
-- under running any runnable leg, and leaves out the set of all legs: the
-- reachable sets are such a family, and every such family contains them.
-- Each Reached atom is one member of that family.
--
-- So any instance the solver finds is truly doomed. It finds every doomed
-- Transaction of n legs once the scope of Reached is at least 2^n (16 for 4
-- legs, 32 for 5). Below that, it can miss one, but never invents one.
sig Reached {
  done: set Leg
}

pred Doomed {
  all disj r, r2: Reached | r.done != r2.done
  all r: Reached | r.done.parents in r.done  -- only sets closed under parents
  some r: Reached | no r.done
  all r: Reached, t: Leg |
    runnable[t, r.done] implies some r2: Reached | r2.done = r.done + t
  no r: Reached | r.done = Leg
}

-- The opposite: a chain of Reached sets from empty to every leg, each one
-- leg more than one before it.
pred CanComplete {
  some r: Reached | no r.done
  some r: Reached | r.done = Leg
  all r: Reached | some r.done implies
    some r0: Reached, t: r.done | r0.done = r.done - t and runnable[t, r0.done]
}

---------------------------------------------------------------------------
-- 5. DOOMED CLASSES. Each is a way a Transaction can pass Preflight and still
--    be doomed. Each is decidable from the DAG and the opening balances,
--    without searching orders.
---------------------------------------------------------------------------

-- A leg into an ONRAMP Wallet (a debit card) is refused whenever it runs.
-- Preflight only reads source Wallets.
pred CreditsOnrampWallet {
  some t: Leg | t.dst.allows = Onramp
}

-- A mint_source leg from a Wallet that doesn't allow onramp. Preflight skips
-- mint_source roots, and validateMintSource only refuses it at dispatch.
pred MintFromUnmintableWallet {
  some t: Minting | t.src.allows in Offramp + NoAllows
}

-- Together the legs take more out of a Wallet than it holds plus everything
-- the Transaction pays in. Every leg must complete, so whatever the order,
-- the last debit finds too little. Preflight checks each root on its own.
-- The simplest case: two roots, each covered by the Wallet, but not both.
pred NetOverdrawn {
  some w: Wallet | balance[w, Leg] < 0
}

-- Legs that leave Wallet w better off: they credit it, and don't debit it
-- too. A leg within w moves nothing on net.
fun gainers[w: Wallet]: set Leg { dst.w - (src.w - Minting) }

-- Some leg can't be covered even at its most favourable moment: its
-- ancestors must have completed, its descendants can't have, and at best
-- every other leg that leaves its source better off has completed too.
-- Preflight only checks roots, where this is the plain balance check. For a
-- leg deeper in the DAG it catches a source funded only by the leg's own
-- descendants, or by nothing at all.
pred UnfundableLeg {
  some t: Leg - Minting |
    let w = t.src |
      plus[balance[w, ancestors[t]],
           (sum l: (Leg - t - ancestors[t] - descendants[t]) & gainers[w] | l.amt)]
        < t.amt
}

-- The legs that take money out of Wallet w, and must all run before anything
-- pays it back: every leg that leaves w better off comes after each of them.
fun debitsBeforeRefill[w: Wallet]: set Leg {
  { d: (src.w - Minting) - dst.w | gainers[w] in descendants[d] }
}

-- Those debits need more than w holds. Each is covered alone, so Preflight
-- passes them if they're roots, but the last of them to run finds w short,
-- and the refill waits for it. The simplest case: two roots each spend all
-- of w, and a third leg, after both, refills it.
pred RefillWaitsOnDebits {
  some w: Wallet | (sum d: debitsBeforeRefill[w] | d.amt) > w.spendable
}

pred KnownDoomedClass {
  CreditsOnrampWallet or MintFromUnmintableWallet or NetOverdrawn or UnfundableLeg
    or RefillWaitsOnDebits
}

---------------------------------------------------------------------------
-- 6. COMMANDS. Every one is a single-state search, with no steps, so each
--    takes seconds.
---------------------------------------------------------------------------

-- The question itself: a Transaction that go/ accepts and that can't
-- complete. There are several.
run DoomedTransaction {
  Preflight and Doomed
} for 4 but 6 Int, 16 Reached expect 1

-- Non-vacuity: some accepted Transaction of four legs does complete, so
-- Preflight and runnable don't rule everything out.
run CompletableTransaction {
  Preflight and CanComplete and #Leg = 4
} for 4 but 6 Int, 5 Reached expect 1

-- Teeth for the certificate: one ordinary leg that its Wallet covers, into a
-- Wallet that takes credits, always completes. Drop the closure line from
-- Doomed and this finds an instance.
run CoveredLegIsNeverDoomed {
  Preflight and Doomed and one Leg and no Minting and no Leg.dst.allows & Onramp
} for 4 but 6 Int, 16 Reached expect 0

-- Each class dooms a Transaction that passes Preflight on its own, not only
-- beside another class.
run DoomedByOnrampCredit {
  Preflight and Doomed and CreditsOnrampWallet
  not (MintFromUnmintableWallet or NetOverdrawn or UnfundableLeg or RefillWaitsOnDebits)
} for 4 but 6 Int, 16 Reached expect 1

run DoomedByUnmintableMint {
  Preflight and Doomed and MintFromUnmintableWallet
  not (CreditsOnrampWallet or NetOverdrawn or UnfundableLeg or RefillWaitsOnDebits)
} for 4 but 6 Int, 16 Reached expect 1

run DoomedByNetOverdraft {
  Preflight and Doomed and NetOverdrawn
  not (CreditsOnrampWallet or MintFromUnmintableWallet or UnfundableLeg or RefillWaitsOnDebits)
} for 4 but 6 Int, 16 Reached expect 1

run DoomedByUnfundableLeg {
  Preflight and Doomed and UnfundableLeg
  not (CreditsOnrampWallet or MintFromUnmintableWallet or NetOverdrawn or RefillWaitsOnDebits)
} for 4 but 6 Int, 16 Reached expect 1

run DoomedByRefillWaitingOnDebits {
  Preflight and Doomed and RefillWaitsOnDebits
  not (CreditsOnrampWallet or MintFromUnmintableWallet or NetOverdrawn or UnfundableLeg)
} for 4 but 6 Int, 16 Reached expect 1

-- The worked example of UnfundableLeg: root R pays Q, A (after R) spends
-- from an empty W, and only B (after A) pays W, out of Q. Every final
-- balance is fine, and it's still doomed.
run FundedOnlyDownstream {
  Preflight and Doomed and UnfundableLeg
  some disj r, a, b: Leg, disj z, q, w, x: Wallet {
    Leg = r + a + b
    no Minting
    r.src = z and r.dst = q and no r.parents
    a.src = w and a.dst = x and a.parents = r
    b.src = q and b.dst = w and b.parents = a
    z.spendable = 1 and q.spendable = 0 and w.spendable = 0
    r.amt = 1 and a.amt = 1 and b.amt = 1
    no (z + q + w + x).allows & Onramp
  }
} for 4 but 6 Int, 8 Reached expect 1

-- The static classes don't cover every doomed Transaction, and no finite
-- list of them will: a refill can wait on one debit through the DAG and on
-- another through a second Wallet's funding, and such chains can run through
-- any number of Wallets. The first one found, at 4 legs, pinned below: W
-- holds 3. Roots A (W -> V) and B (W -> U) each take 2. C (U -> W, after A)
-- is W's only refill, and U is funded only by B. Whichever root runs second
-- finds W short, and the refill can't come first. What decides doom exactly
-- is the search this model does: whether the set of all legs is reachable.
run DoomedBeyondKnownClasses {
  Preflight and Doomed and not KnownDoomedClass
} for 4 but 6 Int, 16 Reached expect 1

run RefillFundedThroughAnotherWallet {
  Preflight and Doomed and not KnownDoomedClass
  some disj a, b, c: Leg, disj w, v, u: Wallet {
    Leg = a + b + c
    no Minting
    a.src = w and a.dst = v and no a.parents
    b.src = w and b.dst = u and no b.parents
    c.src = u and c.dst = w and c.parents = a
    w.spendable = 3 and v.spendable = 0 and u.spendable = 0
    a.amt = 2 and b.amt = 2 and c.amt = 2
    no (w + v + u).allows & Onramp
  }
} for 4 but 6 Int, 8 Reached expect 1
