module moneyflow/ledger

/*
 * The money_flow ledger as a relational model. It says what the ledger
 * does, not how Go stores it: the event store and its concurrency are
 * spec/eventstore.tla's job. Here, a state is every Account's balance plus
 * where every Transfer leg and Transaction stands, and a step is one thing
 * the saga orchestrator, TigerBeetle or the ACH network does.
 *
 * Sources for each fact are cited inline. When one of them changes, this
 * file must change with it.
 */

---------------------------------------------------------------------------
-- 1. STATIC STRUCTURE: parties, account types, and the flags TigerBeetle
--    enforces on them. None of this changes while the system runs.
---------------------------------------------------------------------------

-- A TigerBeetle account either may go negative, or carries
-- debits_must_not_exceed_credits (go/internal/token/allows.go).
-- credits_must_not_exceed_debits (DebitCard, ALLOWS_ONRAMP) is left out:
-- no shape modelled here moves money through a debit card.
abstract sig Cap {}
one sig Uncapped, DebitsCapped extends Cap {}

-- Every party keeps money on two sides of the ledger (ruby/CONTEXT.md,
-- "Cash side and cleared side"): the real money the platform holds, and
-- how much of it may be spent. They are the same money, seen twice.
abstract sig Side {}
one sig CashSide, ClearedSide extends Side {}

abstract sig AccountType {
  cap:  one Cap,   -- the TigerBeetle flag every Token in this Wallet gets
  side: lone Side  -- which side it counts towards; none for claims and
                   -- for the accounts on the bank boundary
}
one sig Bank, BankControl, IssuerControl, Receivable,
        Cash, UnclearedCash, ClearedCash, Investment,
        SecuritySupply, SecurityEscrow, SecurityRepayment, SecurityCash
  extends AccountType {}

-- AccountType#allows (ruby/app/types/enums/account_types.rb:65) maps to
-- AllowsToAccountFlags (go/internal/token/allows.go):
--   Bank, BankControl, IssuerControl, Receivable -> ALLOWS_ONRAMP_AND_OFFRAMP -> no flag
--   everything else                              -> ALLOWS_NONE -> debits capped
-- Uncapped accounts may run negative. Bank's negative is money owed to the
-- outside world, IssuerControl's is the total claims outstanding, and
-- Receivable's is what the entity owes the platform (ruby ADR 0011).
-- Stated per type: `(A + B).cap = X` only says the union of their caps is
-- {X}, which a `lone` field satisfies with one of them left empty.
fact AllowsBecomeFlags {
  all t: Bank + BankControl + IssuerControl + Receivable | t.cap = Uncapped
  all t: Cash + UnclearedCash + ClearedCash + Investment
       + SecuritySupply + SecurityEscrow + SecurityRepayment + SecurityCash | t.cap = DebitsCapped
}

-- Entity: `cash` is the cash side; uncleared + cleared cash is the cleared
-- side. Security: `security_cash` is the cash side; escrow + repayment is
-- the cleared side (ruby ADR 0009, decision 2). A Receivable is on neither
-- side (ruby ADR 0011, decision 4).
fact TwoSides {
  all t: Cash + SecurityCash | t.side = CashSide
  all t: UnclearedCash + ClearedCash + SecurityEscrow + SecurityRepayment | t.side = ClearedSide
  all t: Bank + BankControl + IssuerControl + Receivable + Investment + SecuritySupply | no t.side
}

-- Parties are what money moves between (ruby/CONTEXT.md "Party", "Role").
abstract sig Party {}
abstract sig Entity extends Party {
  owedAtStart: one Int  -- what its Receivable owes when the search starts
}
sig Investor, Borrower, Issuer extends Entity {}
sig Security extends Party {
  issuer:   one Issuer,    -- each Security names its own Issuer
  borrower: one Borrower   -- the one obligation behind it
}

-- One TigerBeetle-backed Wallet, collapsed to a single account. A Wallet
-- of many Tokens behaves like one account for the caps modelled here,
-- because the Transfer's legs split the amount across its Tokens.
sig Account {
  owner: one Party,
  type:  one AccountType,
  var posted:   one Int,  -- credits_posted - debits_posted
  var reserved: one Int   -- debits_pending: what staged Transfers hold back
}

-- Onboarding opens accounts per role, and IssueOffering opens a Security's
-- four (ruby/app/types/enums/account_types.rb, BANKING and the role lists).
fun BANKING: set AccountType { Bank + BankControl + UnclearedCash + ClearedCash + Cash + Receivable }

fact Onboarding {
  all a: Account | a.owner in Investor implies a.type in BANKING + Investment
  all a: Account | a.owner in Borrower implies a.type in BANKING
  all a: Account | a.owner in Issuer   implies a.type in BANKING + IssuerControl
  all a: Account | a.owner in Security implies
    a.type in SecuritySupply + SecurityEscrow + SecurityRepayment + SecurityCash
  -- At most one account per (party, type). accounts_security_type_unique
  -- enforces this for a Security, and Services::Securities::Wallets raises
  -- on a duplicate. Note that Services::Ach::EntityWallets silently keeps
  -- the last one instead; this model assumes the stronger rule.
  all p: Party, t: AccountType | lone a: Account | a.owner = p and a.type = t
}

fun acct[p: Party, t: AccountType]: lone Account {
  { a: Account | a.owner = p and a.type = t }
}

-- TigerBeetle's check for debits_must_not_exceed_credits. The pending
-- debits of a staged Transfer count against the cap, but its pending
-- credits don't count as money the destination has.
fun available[a: Account]: Int { minus[a.posted, a.reserved] }

pred canDebit[a: Account, n: Int] {
  a.type.cap = Uncapped or minus[available[a], n] >= 0
}

---------------------------------------------------------------------------
-- 2. TRANSFERS AND TRANSACTIONS. A Transaction is a DAG of Transfers (go
--    ADR 0001, go/internal/transaction/dag.go). Each Transfer here stands
--    for one money movement between two Wallets.
---------------------------------------------------------------------------

-- A Transfer's life, as the saga sees it (proto/transfer/v1, and
-- childState in go ADR 0002):
--   Idle           not yet dispatched
--   Pending        staged in TigerBeetle, waiting on the ACH network
--   Posted         committed: the money has moved
--   Failed         refused by a cap, failed in Go, or returned (R-code)
--   Cancelled      abandoned or voided by a rollback before it posted
--   Reversed       posted, then undone by a committed Reversal
--   ReversalFailed posted, and its Reversal was refused
abstract sig TState {}
one sig Idle, Pending, Posted, Failed, Cancelled, Reversed, ReversalFailed extends TState {}

sig Transfer {
  txn:     one Txn,
  src:     one Account,
  dst:     one Account,
  amt:     one Int,
  parents: set Transfer,  -- transfer_dependency: which legs must post first
  var st:  one TState
}

-- The legs that are staged: ACH real legs, which the network settles over
-- days (Services::Ach::TransactionShape::LEGS, stage: true).
sig Staged in Transfer {}

-- TransactionState (proto/transaction/v1/transaction.proto:239). Rejected
-- is left out. A pre-flight rejection writes nothing, which the model
-- treats the same as a Transaction that never starts. Initialized and
-- Started are folded into one state.
abstract sig XState {}
one sig NotStarted, Started, RollingBack, Completed, RolledBack, RollbackFailed extends XState {}

abstract sig Txn {
  amount:   one Int,
  waitsFor: lone Txn,  -- a sweep that only starts once another Transaction
                       -- has completed (Clearing, Disbursement)
  var xs:   one XState
}

fun legs[x: Txn]: set Transfer { txn.x }

-- What validateDAG enforces before TransactionInitialized is written:
-- positive amounts, no reference outside the Transaction, and no cycle.
-- Amounts are kept small so that balances stay inside Alloy's bounded Int.
fact WellFormedDag {
  all t: Transfer {
    t.src != t.dst
    t.amt > 0
    t.parents in legs[t.txn]
    t not in t.^parents
  }
  all x: Txn | x.amount > 0 and x.amount <= 2
}

---------------------------------------------------------------------------
-- 3. SHAPES: the factories Ruby asks Go to run. Each one fixes a
--    Transaction's legs, their Wallets, and their order.
---------------------------------------------------------------------------

pred leg[t: Transfer, x: Txn, s, d: Account, n: Int] {
  t.txn = x and t.src = s and t.dst = d and t.amt = n
}

abstract sig Ach extends Txn { holder: one Entity, real: one Transfer, shadow: one Transfer }
sig Deposit, Withdrawal extends Ach {}
sig Clearing extends Txn { deposit: one Deposit, clearLeg: one Transfer }
sig Offering extends Txn { offered: one Security, mint: one Transfer }
abstract sig SecuritiesMoney extends Txn { sec: one Security, money: one Transfer, cashLeg: one Transfer }
sig Purchase extends SecuritiesMoney { buyer: one Investor, claim: one Transfer }
sig Draw, Repayment extends SecuritiesMoney {}
sig Disbursement extends SecuritiesMoney { payee: one Investor, principal: one Int, retire: lone Transfer }
-- ruby ADR 0011: a late return is its own Transaction, in one of three forms,
-- and Recovery collects what a debt return left owed.
abstract sig LateReturn extends Txn { returnOf: one Ach }
sig WithdrawalReturn extends LateReturn { toCash: one Transfer, toCleared: one Transfer }
sig Clawback extends LateReturn { fromUncleared: one Transfer, fromCash: one Transfer }
sig DebtReturn extends LateReturn { owedLeg: one Transfer }
sig Recovery extends Txn { debtor: one Entity, fromCleared: one Transfer, toReceivable: one Transfer }

-- ruby/app/services/ach/transaction_shape.rb and clearing_shape.rb.
fact AchShapes {
  Staged = Ach.real
  -- Deposit: the real leg moves bank -> cash and is staged. The shadow leg
  -- moves bank_control -> uncleared_cash and waits for the real leg to
  -- post, so uncleared cash is only minted for money that arrived.
  all x: Deposit {
    leg[x.real,   x, acct[x.holder, Bank],        acct[x.holder, Cash],          x.amount]
    leg[x.shadow, x, acct[x.holder, BankControl], acct[x.holder, UnclearedCash], x.amount]
    no x.real.parents
    x.shadow.parents = x.real
    legs[x] = x.real + x.shadow
    no x.waitsFor
  }
  -- Withdrawal, version 2 (ruby ADR 0004): Funding (the shadow leg,
  -- cleared_cash -> bank_control) runs first. The real leg (cash -> bank,
  -- staged) waits for it, so the cleared cash has already left before any
  -- money goes out over ACH.
  all x: Withdrawal {
    leg[x.shadow, x, acct[x.holder, ClearedCash], acct[x.holder, BankControl], x.amount]
    leg[x.real,   x, acct[x.holder, Cash],        acct[x.holder, Bank],        x.amount]
    no x.shadow.parents
    x.real.parents = x.shadow
    legs[x] = x.real + x.shadow
    no x.waitsFor
  }
  -- Clearing (ruby ADR 0003) moves uncleared -> cleared cash, and is started
  -- by a sweep only after its deposit's Transaction completed. There is one
  -- Clearing per deposit, because its id is derived from the deposit's.
  all x: Clearing {
    leg[x.clearLeg, x, acct[x.deposit.holder, UnclearedCash],
                       acct[x.deposit.holder, ClearedCash], x.amount]
    x.amount = x.deposit.amount
    no x.clearLeg.parents
    legs[x] = x.clearLeg
    x.waitsFor = x.deposit
  }
  all d: Deposit | lone deposit.d
}

-- ruby/app/services/securities/*_shape.rb, version 3. Legs as ruby ADR 0009
-- lays them out, ordered as ruby ADR 0010 does: an entity's cleared cash
-- leaves before its cash, and its cash arrives before its cleared cash, so
-- its cash always covers its cleared cash.
fact SecuritiesShapes {
  -- Supply is minted once, from the Issuer's issuer_control (ruby ADR 0006).
  all x: Offering {
    leg[x.mint, x, acct[x.offered.issuer, IssuerControl], acct[x.offered, SecuritySupply], x.amount]
    no x.mint.parents
    legs[x] = x.mint
    no x.waitsFor
  }
  all s: Security | lone offered.s
  -- Purchase. The claim leg is the root, so an oversubscription is refused
  -- before any Investor money moves. Then the money leg, then its cash leg:
  -- the Investor pays.
  all x: Purchase {
    leg[x.claim,   x, acct[x.sec, SecuritySupply], acct[x.buyer, Investment],   x.amount]
    leg[x.money,   x, acct[x.buyer, ClearedCash],  acct[x.sec, SecurityEscrow], x.amount]
    leg[x.cashLeg, x, acct[x.buyer, Cash],         acct[x.sec, SecurityCash],   x.amount]
    no x.claim.parents
    x.money.parents = x.claim
    x.cashLeg.parents = x.money
    legs[x] = x.claim + x.money + x.cashLeg
    no x.waitsFor
  }
  -- Draw: an operator moves escrowed money to the Borrower, on both sides.
  -- The Borrower is paid, so the cash leg runs first.
  all x: Draw {
    leg[x.money,   x, acct[x.sec, SecurityEscrow], acct[x.sec.borrower, ClearedCash], x.amount]
    leg[x.cashLeg, x, acct[x.sec, SecurityCash],   acct[x.sec.borrower, Cash],        x.amount]
    no x.cashLeg.parents
    x.money.parents = x.cashLeg
    legs[x] = x.money + x.cashLeg
    no x.waitsFor
  }
  -- Repayment: the Borrower pays into the Security, on both sides, money leg
  -- first.
  all x: Repayment {
    leg[x.money,   x, acct[x.sec.borrower, ClearedCash], acct[x.sec, SecurityRepayment], x.amount]
    leg[x.cashLeg, x, acct[x.sec.borrower, Cash],        acct[x.sec, SecurityCash],      x.amount]
    no x.money.parents
    x.cashLeg.parents = x.money
    legs[x] = x.money + x.cashLeg
    no x.waitsFor
  }
  -- Disbursement (ruby ADR 0007) is one holder's share of one Repayment.
  -- Retirement (investment -> issuer_control) is the root, so a holder is
  -- never paid principal they don't hold. The Investor is paid, so the cash
  -- leg comes next, then the payout. With no principal to retire, there's no
  -- retirement leg, and the cash leg is the root.
  all x: Disbursement {
    x.principal >= 0 and x.principal <= x.amount
    (x.principal > 0) iff (some x.retire)
    some x.retire implies
      leg[x.retire, x, acct[x.payee, Investment], acct[x.sec.issuer, IssuerControl], x.principal]
    leg[x.money,   x, acct[x.sec, SecurityRepayment], acct[x.payee, ClearedCash], x.amount]
    leg[x.cashLeg, x, acct[x.sec, SecurityCash],      acct[x.payee, Cash],        x.amount]
    no x.retire.parents
    x.cashLeg.parents = x.retire
    x.money.parents = x.cashLeg
    legs[x] = x.retire + x.money + x.cashLeg
    one x.waitsFor and x.waitsFor in Repayment and x.waitsFor.sec = x.sec
  }
}

-- ruby/app/services/ach/late_return_shape.rb and recovery_shape.rb, all
-- version 1, ordered as ruby ADR 0010 orders every shape: an entity's
-- cleared cash leaves before its cash, and its cash arrives before its
-- cleared cash.
fact LateReturnShapes {
  -- One late return per ACH Transaction, for its whole amount: its id is
  -- derived from the ACH Transaction's. It starts only once that one has
  -- completed; a notice for one still running cancels its staged real leg
  -- instead (the `returned` step).
  all x: LateReturn {
    x.amount = x.returnOf.amount
    x.waitsFor = x.returnOf
  }
  all a: Ach | lone returnOf.a
  -- Withdrawal return: the money comes back, cash first.
  all x: WithdrawalReturn {
    x.returnOf in Withdrawal
    leg[x.toCash,    x, acct[x.returnOf.holder, Bank],        acct[x.returnOf.holder, Cash],        x.amount]
    leg[x.toCleared, x, acct[x.returnOf.holder, BankControl], acct[x.returnOf.holder, ClearedCash], x.amount]
    no x.toCash.parents
    x.toCleared.parents = x.toCash
    legs[x] = x.toCash + x.toCleared
  }
  -- Clawback: a deposit whose Clearing was never recorded is taken back
  -- whole, uncleared cash first.
  all x: Clawback {
    x.returnOf in Deposit
    leg[x.fromUncleared, x, acct[x.returnOf.holder, UnclearedCash], acct[x.returnOf.holder, BankControl], x.amount]
    leg[x.fromCash,      x, acct[x.returnOf.holder, Cash],          acct[x.returnOf.holder, Bank],        x.amount]
    no x.fromUncleared.parents
    x.fromCash.parents = x.fromUncleared
    legs[x] = x.fromUncleared + x.fromCash
  }
  -- Debt return: a deposit whose Clearing was recorded is owed, whatever the
  -- depositor still holds. It mints out of the Receivable.
  all x: DebtReturn {
    x.returnOf in Deposit
    leg[x.owedLeg, x, acct[x.returnOf.holder, Receivable], acct[x.returnOf.holder, Bank], x.amount]
    no x.owedLeg.parents
    legs[x] = x.owedLeg
  }
  -- Recovery: the debtor pays what it owes from cleared cash, cleared cash
  -- first, like a Repayment.
  all x: Recovery {
    leg[x.fromCleared,  x, acct[x.debtor, ClearedCash], acct[x.debtor, BankControl], x.amount]
    leg[x.toReceivable, x, acct[x.debtor, Cash],        acct[x.debtor, Receivable],  x.amount]
    no x.fromCleared.parents
    x.toReceivable.parents = x.fromCleared
    legs[x] = x.fromCleared + x.toReceivable
    no x.waitsFor
  }
}

---------------------------------------------------------------------------
-- 4. INITIAL STATE
---------------------------------------------------------------------------

fun sideTotal[p: Party, s: Side]: Int {
  sum a: { a: Account | a.owner = p and a.type.side = s } | a.posted
}

fun bal[p: Party, t: AccountType]: Int { sum a: acct[p, t] | a.posted }

-- What the network has moved for an entity that the cleared side doesn't
-- mirror and the Receivable doesn't owe (ruby ADR 0011, decision 4). Every
-- shape that moves `bank` moves `bank_control` or the Receivable by the same
-- amount, so at rest this is 0: every late return was recorded.
fun unrecorded[e: Entity]: Int {
  plus[minus[bal[e, Bank], bal[e, BankControl]], bal[e, Receivable]]
}

-- What an entity owes, as Services::Receivable reads it: debt returns the
-- projection has seen complete, less the Recoveries it has seen complete.
-- One still in flight doesn't count yet.
fun owed[e: Entity]: Int {
  minus[plus[e.owedAtStart, (sum d: { d: DebtReturn | d.returnOf.holder = e and d.xs = Completed } | d.amount)],
        (sum r: { r: Recovery | r.debtor = e and r.xs = Completed } | r.amount)]
}

-- The search starts from any balances the ledger could plausibly hold,
-- rather than from zero. Replaying every deposit and clearing from zero
-- would push short counterexamples past the step bound. This
-- over-approximates the reachable states, so a counterexample must be
-- confirmed reachable (by a Ruby or Go test) before anything is filed.
pred OpeningBalances {
  all a: Account {
    a.reserved = 0                                  -- nothing staged yet
    a.posted >= -4 and a.posted <= 4                -- keeps Int arithmetic in range
    a.type.cap = DebitsCapped implies a.posted >= 0 -- TigerBeetle would never allow less
  }
  (sum a: Account | a.posted) = 0                   -- double entry: every debit has its credit
  all p: Party | sideTotal[p, CashSide] = sideTotal[p, ClearedSide]
  all e: Entity {
    unrecorded[e] = 0
    bal[e, Receivable] <= 0
    e.owedAtStart = minus[0, bal[e, Receivable]]
  }
  -- A deposit that completed before the search may already have had its
  -- Clearing recorded, though not started. No notice has arrived yet.
  ClearingRecorded in AlreadyCompleted
  no Noticed
  all t: Transfer | t.st = (t.txn in AlreadyCompleted => Posted else Idle)
  all x: Txn | x.xs = (x in AlreadyCompleted => Completed else NotStarted)
}

-- Deposits that completed before the search starts. The opening balances
-- already stand in for the ledger's history. These give part of that history
-- names, so a later action (a late ACH return) can refer to a deposit without
-- the trace first replaying all of its steps. They're still uncleared: any
-- Clearing runs inside the trace. So their holder's uncleared cash must still
-- hold them, because uncleared money can't have been spent. Without that, the
-- opening balances could contradict the history they stand in for.
sig AlreadyCompleted in Deposit {}

fact HistoryIsConsistent {
  all e: Entity |
    (sum d: AlreadyCompleted & holder.e | d.amount)
      <= (sum a: { a: Account | a.owner = e and a.type = UnclearedCash } | a.posted)
}

---------------------------------------------------------------------------
-- 5. TRANSITIONS. Each step is exactly one of the actions below. Anything
--    that can happen in any order in the real system can happen in any
--    order here too: the saga is event-triggered, and the ACH network
--    answers whenever it answers.
---------------------------------------------------------------------------

-- The saga dispatches a leg once its Transaction has started and every
-- parent has posted (go ADR 0012: a ready child is always requested).
pred ready[t: Transfer] {
  t.st = Idle
  t.txn.xs = Started
  t.parents.st in Posted
}

-- What Ruby checks, on its own record, before it asks Go for a Transaction
-- (ruby ADR 0011, decisions 3 and 5):
-- - a Clearing, only once Clear has recorded it;
-- - a late return, only for an entry with a notice. A deposit is clawed back
--   only if its Clearing was never recorded, and owed otherwise;
-- - a Recovery, only while no other of the debtor's is in flight, and for no
--   more than it owes.
pred mayBegin[x: Txn] {
  x in Clearing implies x.deposit in ClearingRecorded
  x in LateReturn implies x.returnOf in Noticed
  x in Clawback implies x.returnOf not in ClearingRecorded
  x in DebtReturn implies x.returnOf in ClearingRecorded
  x in Recovery implies {
    no r: Recovery - x | r.debtor = x.debtor and r.xs in Started + RollingBack
    x.amount <= owed[x.debtor]
  }
}

-- Ruby originates a Transaction. Sweeps start one only after the
-- Transaction it waits for has completed.
pred begin[x: Txn] {
  x.xs = NotStarted
  x.waitsFor.xs in Completed
  mayBegin[x]
  xs' = xs ++ (x -> Started)
  st' = st
  posted' = posted
  reserved' = reserved
}

-- An unstaged leg commits in one TigerBeetle call, provided the cap on its
-- source allows the debit.
pred post[t: Transfer] {
  ready[t]
  t not in Staged
  canDebit[t.src, t.amt]
  st' = st ++ (t -> Posted)
  posted' = posted ++ (t.src -> minus[t.src.posted, t.amt])
                   ++ (t.dst -> plus[t.dst.posted, t.amt])
  reserved' = reserved
  xs' = xs
}

-- A staged leg becomes a pending TigerBeetle transfer. Its debit is
-- reserved now and checked against the cap now.
pred stage[t: Transfer] {
  ready[t]
  t in Staged
  canDebit[t.src, t.amt]
  st' = st ++ (t -> Pending)
  reserved' = reserved ++ (t.src -> plus[t.src.reserved, t.amt])
  posted' = posted
  xs' = xs
}

-- Settlement: the provider reports the entry posted, and the pending
-- transfer is posted.
pred settle[t: Transfer] {
  t.st = Pending
  st' = st ++ (t -> Posted)
  posted' = posted ++ (t.src -> minus[t.src.posted, t.amt])
                   ++ (t.dst -> plus[t.dst.posted, t.amt])
  reserved' = reserved ++ (t.src -> minus[t.src.reserved, t.amt])
  xs' = xs
}

-- Return: an R-code, or a refusal at submission. The pending transfer is
-- voided and its reservation released.
pred returned[t: Transfer] {
  t.st = Pending
  st' = st ++ (t -> Failed)
  reserved' = reserved ++ (t.src -> minus[t.src.reserved, t.amt])
  posted' = posted
  xs' = xs
}

-- A ready leg may fail for any reason: TigerBeetle refusing the cap (the
-- only way out when canDebit is false), Go failing, or a pre-flight
-- rejection. The environment is adversarial, as in spec/eventstore.tla.
pred fail[t: Transfer] {
  ready[t]
  st' = st ++ (t -> Failed)
  posted' = posted
  reserved' = reserved
  xs' = xs
}

pred complete[x: Txn] {
  x.xs = Started
  legs[x].st in Posted
  xs' = xs ++ (x -> Completed)
  st' = st
  posted' = posted
  reserved' = reserved
}

-- A failed leg starts the rollback in the same commit (go ADR 0013). Legs
-- that never posted are cancelled synchronously, with their reservations
-- released (ROLLBACK_METHOD_CANCELLED or _ABANDONED).
pred startRollback[x: Txn] {
  x.xs = Started
  Failed in legs[x].st
  xs' = xs ++ (x -> RollingBack)
  st' = st ++ ((legs[x] & st.(Idle + Pending)) -> Cancelled)
  all a: Account |
    a.reserved' = minus[a.reserved, (sum l: legs[x] & st.Pending & src.a | l.amt)]
  posted' = posted
}

-- Rollback is DAG-ordered (go ADR 0002): a posted leg is reversed only
-- once none of its children remains posted.
pred reversible[t: Transfer] {
  t.txn.xs = RollingBack
  t.st = Posted
  no c: legs[t.txn] | t in c.parents and c.st = Posted
}

-- A Reversal is a new Transfer from dst back to src, so the destination's
-- cap applies to it. If the destination has already spent the money, the
-- Reversal is refused.
-- Simplification: the Reversal commits or fails in one step. In the real
-- system it runs its own saga, and inherits staging.
pred reverse[t: Transfer] {
  reversible[t]
  reserved' = reserved
  xs' = xs
  canDebit[t.dst, t.amt] implies {
    st' = st ++ (t -> Reversed)
    posted' = posted ++ (t.dst -> minus[t.dst.posted, t.amt])
                     ++ (t.src -> plus[t.src.posted, t.amt])
  } else {
    st' = st ++ (t -> ReversalFailed)
    posted' = posted
  }
}

-- Once every leg is resolved, the Transaction reaches its terminal state.
-- TransactionRollbackFailed is the state that needs a person.
pred finishRollback[x: Txn] {
  x.xs = RollingBack
  legs[x].st in Failed + Cancelled + Reversed + ReversalFailed
  xs' = xs ++ (x -> ((ReversalFailed in legs[x].st) => RollbackFailed else RolledBack))
  st' = st
  posted' = posted
  reserved' = reserved
}

pred stutter {
  st' = st
  posted' = posted
  reserved' = reserved
  xs' = xs
}

fact Behaviour {
  OpeningBalances
  always (
    stutter
    or (some x: Txn | begin[x] or complete[x] or startRollback[x] or finishRollback[x])
    or (some t: Transfer | post[t] or stage[t] or settle[t] or returned[t] or fail[t] or reverse[t])
  )
}

-- Ruby's own record, which decides what it asks Go for. Both are written
-- under a lock on the ACH Transaction's row, so they never change in the
-- same step (ruby ADR 0011, decision 3). Neither moves money, so each is a
-- `stutter` step for the ledger.
--
-- A notice for a completed ACH Transaction is a late return: the network
-- takes the money back after the entry settled. An unauthorized-debit return
-- (R05, R07, R10) may arrive up to 60 days after settlement, long after
-- ClearingPolicy's 3 business days. The first notice wins.
var sig Noticed in Ach {}
-- Clear records a deposit's Clearing ids before it asks Go, and refuses a
-- deposit with a notice.
var sig ClearingRecorded in Deposit {}

pred notice[a: Ach] {
  a.xs = Completed
  a not in Noticed
  stutter
  Noticed' = Noticed + a
  ClearingRecorded' = ClearingRecorded
}

pred recordClearing[d: Deposit] {
  d.xs = Completed
  d not in ClearingRecorded
  d not in Noticed
  stutter
  ClearingRecorded' = ClearingRecorded + d
  Noticed' = Noticed
}

fact RubyRecords {
  always (
    (Noticed' = Noticed and ClearingRecorded' = ClearingRecorded)
    or (some a: Ach | notice[a])
    or (some d: Deposit | recordClearing[d])
  )
}

---------------------------------------------------------------------------
-- 6. PROPERTIES
---------------------------------------------------------------------------

fun touching[p: Party]: set Txn { { x: Txn | p in legs[x].(src + dst).owner } }

-- A party is at rest when none of the Transactions that move its money is
-- in flight. RollbackFailed doesn't count as at rest: it's the escalation
-- state, where a person reconciles by hand.
pred atRest[p: Party] { touching[p].xs in NotStarted + Completed + RolledBack }

-- SAFETY 1: the cash side and the cleared side agree whenever nothing is
-- in flight (ruby ADR 0009, "Money that moves between two parties inside
-- the platform moves on both sides, by the same amount"). For an entity,
-- cash = uncleared + cleared cash. For a Security, security_cash =
-- escrow + repayment. If this breaks, some shape moves money on one side
-- only. Purchase v1, which had no cash leg, is the bug it would have
-- caught. To confirm the check has teeth, delete the cash leg from
-- Purchase and it should fail.
assert SidesAgreeAtRest {
  always all p: Party |
    atRest[p] implies sideTotal[p, CashSide] = sideTotal[p, ClearedSide]
}

-- SAFETY 2: a funded withdrawal can always be paid (ruby ADR 0004,
-- ruby/CONTEXT.md "Cash side and cleared side"). Once Funding has moved
-- the cleared cash out, the real leg must not be refused by the cap on
-- `cash`. If it is, the entity was allowed to spend cleared cash that had
-- no cash behind it at that moment.
-- With the cash leg beside the money leg (shapes version 2), a concurrent
-- Repayment's cash leg could take the cash after Funding had committed
-- (namelessnotion/money_flow#7). Ordering the legs (ruby ADR 0010) closes it.
assert FundedWithdrawalCanBePaid {
  always all w: Withdrawal |
    (w.xs = Started and w.shadow.st = Posted and w.real.st = Idle)
      implies canDebit[w.real.src, w.amount]
}

-- ADVERSARIAL 1: uncleared money never leaves an entity. Its available cash
-- (posted, less what staged withdrawals have reserved) always covers its
-- uncleared and cleared cash together. Every way money leaves `cash`, whether
-- a withdrawal's real leg or a cash leg paying a Security, first takes the
-- same amount out of *cleared* cash. So while this holds, the part of `cash`
-- that backs uncleared money can't be spent or sent to a bank. It's also the
-- in-flight invariant ruby ADR 0010 relies on: FundedWithdrawalCanBePaid
-- follows from it.
assert UnclearedMoneyStaysBacked {
  always all a: Account |
    a.type = Cash implies available[a] >= sideTotal[a.owner, ClearedSide]
}

-- ADVERSARIAL 2: a late return can be recovered from the depositor. When the
-- network takes a deposit back, the depositor's available cash still covers
-- it, so the platform could debit it from them. A counterexample means the
-- money was spent or withdrawn before the return arrived, and the platform
-- carries the loss. It needn't be a second party: the account owner can
-- withdraw the cleared money back to the same account, then dispute the
-- original debit as unauthorized, and receive the money twice.
-- Since ruby ADR 0011 the loss is recorded, as owed, but nothing prevents
-- it. That is the exposure policy's job.
assert LateReturnIsRecoverable {
  always all d: Deposit |
    notice[d] implies canDebit[acct[d.holder, Cash], d.amount]
}

-- SAFETY 3: the ledger can always record a late return (ruby ADR 0011,
-- decision 2). A withdrawal return and a debt return mint out of uncapped
-- accounts. A clawback's legs are covered because its deposit's Clearing was
-- never recorded, so nothing took the deposit's money out of uncleared cash,
-- and UnclearedMoneyStaysBacked keeps `cash` covering it. Worded over ready
-- legs, because the adversarial `fail` step can refuse any leg for reasons
-- that aren't money.
assert LateReturnIsAlwaysFunded {
  always all t: legs[LateReturn] | ready[t] implies canDebit[t.src, t.amt]
}

-- SAFETY 4: Recovery never takes more than is owed, so no Receivable ever
-- holds money (ruby ADR 0011, decision 5).
assert ReceivableNeverPositive {
  always all a: Account | a.type = Receivable implies a.posted <= 0
}

-- SAFETY 5: every dollar the network moved is either mirrored on the cleared
-- side or owed, once nothing of the entity's is in flight (ruby ADR 0011,
-- decision 4).
assert NetworkMoneyIsRecorded {
  always all e: Entity | atRest[e] implies unrecorded[e] = 0
}

-- Weak fairness. The orchestrator keeps reacting to events and resuming,
-- so an action that stays enabled eventually happens (go ADR 0001, 0003).
-- The ACH network eventually settles or returns every entry.
pred Fairness {
  all t: Transfer {
    (eventually always ready[t])       implies (always eventually (post[t] or stage[t] or fail[t]))
    (eventually always t.st = Pending) implies (always eventually (settle[t] or returned[t]))
    (eventually always reversible[t])  implies (always eventually reverse[t])
  }
  all x: Txn {
    (eventually always (x.xs = Started and legs[x].st in Posted))
      implies (always eventually complete[x])
    (eventually always (x.xs = Started and Failed in legs[x].st))
      implies (always eventually startRollback[x])
    (eventually always (x.xs = RollingBack
                        and legs[x].st in Failed + Cancelled + Reversed + ReversalFailed))
      implies (always eventually finishRollback[x])
  }
}

-- LIVENESS: every Transaction that starts reaches a terminal state:
-- Completed, RolledBack, or RollbackFailed, which hands it to a person. A
-- lasso counterexample is a Transaction stuck for ever, such as a rollback
-- that can never resolve its last leg.
assert EveryStartedTransactionConcludes {
  Fairness implies
    all x: Txn | always (x.xs = Started implies
                         eventually x.xs in Completed + RolledBack + RollbackFailed)
}

---------------------------------------------------------------------------
-- 7. COMMANDS. The `run`s are witnesses that each scenario is reachable,
--    so a passing check is not vacuously true.
---------------------------------------------------------------------------

check SidesAgreeAtRest
  for 3 but 6 Int, 8 Account, 5 Transfer, 2 Txn, 1..8 steps

-- The scope the version 2 counterexample needed: one Repayment beside one
-- withdrawal. On 2026-09-26, with glucose, the version 2 shapes pinned to
-- exactly that cast (6 Accounts, 4 Transfers, 1 Repayment, 1 Withdrawal)
-- gave the counterexample in about 11 minutes: the Repayment's cash leg
-- posted before its money leg, then Funding. Version 3 is UNSAT at this
-- scope, which contains that cast: 26 minutes on one run, 6 on another. Solve
-- times vary that much between runs.
check FundedWithdrawalCanBePaid
  for 3 but 6 Int, 6 Account, 4 Transfer, 2 Txn, 1..6 steps expect 0

check EveryStartedTransactionConcludes
  for 3 but 6 Int, 8 Account, 5 Transfer, 2 Txn, 1..14 steps

run DepositSettlesThenClears {
  some c: Clearing | eventually c.xs = Completed
} for 3 but 6 Int, 6 Account, 3 Transfer, 2 Txn, 1..12 steps expect 1

run WithdrawalPaidOut {
  some w: Withdrawal | eventually w.xs = Completed
} for 3 but 6 Int, 6 Account, 2 Transfer, 1 Txn, 1..8 steps expect 1

-- With its legs side by side (version 2), a Draw could end RollbackFailed:
-- its money leg posted and its cash leg failed, and the Borrower withdrew the
-- cleared cash before the rollback could reverse it. With the cash leg
-- first, the Borrower is only ever credited cash they can't spend yet, so
-- reversing it can't be refused.
assert DrawRollbackNeverFails {
  always all d: Draw | d.xs != RollbackFailed
}

check DrawRollbackNeverFails
  for 3 but 6 Int, 6 Account, 4 Transfer, 2 Txn, 1..8 steps expect 0

-- One entity's deposit, its clearing and a withdrawal: every ACH way into
-- and out of `cash`.
check UnclearedMoneyStaysBacked
  for 3 but 6 Int, exactly 5 Account, exactly 5 Transfer,
    exactly 1 Deposit, exactly 1 Clearing, exactly 1 Withdrawal, 1..7 steps expect 0

-- The first-party fraud: a deposit clears and is withdrawn, and then the
-- network returns it. `expect 1` pins the known gap
-- (namelessnotion/money_flow#8). Ruby ADR 0011 records the loss as owed but
-- doesn't prevent it, so this stays `expect 1` until an exposure policy for
-- unauthorized returns lands. On 2026-09-27 the counterexample took about 24
-- minutes, and UnclearedMoneyStaysBacked's proof about 16.5, while the
-- withdrawal-v1 mutation fails it in 14 seconds. With ADR 0011's model, and a
-- Clearing that may already be recorded at the start, they took about 14 and 6.
check LateReturnIsRecoverable
  for 3 but 6 Int, exactly 5 Account, exactly 5 Transfer,
    exactly 1 Deposit, exactly 1 Clearing, exactly 1 Withdrawal, 1..8 steps expect 1

-- One entity's deposit, its Clearing and a late return of either deposit
-- form. To show the check has teeth, drop `x.returnOf not in
-- ClearingRecorded` from mayBegin: a clawback after the Clearing then finds
-- uncleared cash empty. On 2026-09-27 that mutation failed in 28 seconds, and
-- the proof took about a minute.
check LateReturnIsAlwaysFunded
  for 3 but 6 Int, exactly 6 Account, 5 Transfer,
    exactly 1 Deposit, exactly 1 Clearing, exactly 1 LateReturn, 0 Withdrawal, 0 Recovery,
    1..7 steps expect 0

-- Two Recoveries from one debtor. To show the check has teeth, drop the
-- one-in-flight rule from mayBegin: both start while the debtor owes 1, and
-- both are paid (a counterexample in about 2.5 minutes on 2026-09-27; the
-- proof took about 4.5). The fifth Account is `bank`: with only four, the
-- opening balances leave the debtor too little cleared cash to pay twice, and
-- the mutation passes too.
check ReceivableNeverPositive
  for 3 but 6 Int, exactly 5 Account, exactly 4 Transfer, exactly 2 Recovery, 1..8 steps expect 0

-- A deposit owed by a debt return, and a Recovery of it. About 3 minutes.
check NetworkMoneyIsRecorded
  for 3 but 6 Int, exactly 6 Account, exactly 5 Transfer,
    exactly 1 Deposit, exactly 1 DebtReturn, exactly 1 Recovery, 1..7 steps expect 0

-- ruby ADR 0010's guarantee with a Recovery beside a withdrawal: Recovery
-- pays cleared cash first, like a Repayment. Under a minute.
check FundedWithdrawalCanBePaidBesideRecovery {
  always all w: Withdrawal |
    (w.xs = Started and w.shadow.st = Posted and w.real.st = Idle)
      implies canDebit[w.real.src, w.amount]
} for 3 but 6 Int, exactly 5 Account, exactly 4 Transfer,
    exactly 1 Recovery, exactly 1 Withdrawal, 1..6 steps expect 0

-- A clawback beside a withdrawal doesn't let uncleared money leave either.
-- About 5.5 minutes.
check UnclearedMoneyStaysBackedBesideClawback {
  always all a: Account |
    a.type = Cash implies available[a] >= sideTotal[a.owner, ClearedSide]
} for 3 but 6 Int, exactly 5 Account, exactly 6 Transfer,
    exactly 1 Deposit, exactly 1 Clawback, exactly 1 Withdrawal, 1..7 steps expect 0

run WithdrawalReturnRecorded {
  some x: WithdrawalReturn | eventually x.xs = Completed
} for 3 but 6 Int, exactly 5 Account, exactly 4 Transfer,
    exactly 1 Withdrawal, exactly 1 WithdrawalReturn, 1..12 steps expect 1

run DepositClawedBack {
  some x: Clawback | eventually x.xs = Completed
} for 3 but 6 Int, exactly 5 Account, exactly 4 Transfer,
    exactly 1 Deposit, exactly 1 Clawback, 1..6 steps expect 1

-- The slowest witness: about 18 minutes on 2026-09-27.
run DebtRecovered {
  some x: DebtReturn, r: Recovery |
    eventually (x.xs = Completed and r.xs = Completed and bal[r.debtor, Receivable] = 0)
} for 3 but 6 Int, exactly 6 Account, exactly 5 Transfer,
    exactly 1 Deposit, exactly 1 DebtReturn, exactly 1 Recovery, 1..10 steps expect 1
