# Business backend

Ruby holds the business side of money flow: who the entities are, what they asked for, and a read model of
what the ledger did about it. It asks Go to *accept* work and learns what became of it from the published
events; no call it makes runs a ledger operation to completion
([go ADR 0006](../go/docs/adr/0006-synchronous-dispatch-removed-from-the-rpc-surface.md)). Two capabilities live here — moving money across the bank boundary over **ACH**,
and offering **Securities** in a Borrower's debt obligation for Investors to buy fractions of. It originates work in `go/` through its Twirp services and learns the outcome
from `transfer-events` / `transaction-events` / `token-events` ([ADR 0001](docs/adr/0001-read-model-consumer-and-follow-on-write-back.md)).
Go is the source of truth for every Transfer and Transaction; Ruby's view of them may lag and is never
authoritative.

## Glossary

**ACH Transaction**
A deposit or withdrawal between an entity's linked bank account and the platform. It is a Go Transaction
built by the `ach_deposit` or `ach_withdrawal` factory (version `1`), with exactly two legs. Ruby records its
intent in `ach_transactions`, and its lifecycle comes from the projection.

**Real leg**
The Transfer that moves money across the bank boundary: `bank → cash` on a deposit, `cash → bank` on a
withdrawal. It is always staged, because the ACH network settles in days, not in the request.

**Shadow leg**
The Transfer that records the same movement in the clearing accounts: `bank_control → uncleared_cash` on a
deposit, `cleared_cash → bank_control` on a withdrawal. On a deposit it depends on the real leg, and runs only
once the real leg has posted. On a withdrawal it runs first: see *Funding*.

**Funding**
A withdrawal's shadow leg, which moves its amount out of cleared cash into bank control before the real leg
is even staged. Without enough cleared cash the whole Transaction is refused outright before anything reaches
the provider — Go catches the common case in its own accept/reject decision, before writing anything, so
nothing is ever started to roll back ([ADR 0004](docs/adr/0004-ach-withdrawal-funds-before-it-leaves.md);
[go ADR 0004](../go/docs/adr/0004-transaction-accept-time-funding-preflight.md)). Because the real leg waits
on it, the real leg reaching *staged* is itself proof that Funding happened — which is what lets Submission
be safe without watching the Transaction run.

**Submission**
Handing the entry to the ACH provider. Once the provider holds it, the staged real leg is confirmed, and it
waits as *pending*.

Done by a scheduled sweep, not by the call that originated the Transaction, and only once the real leg is
*staged* — which for a withdrawal is unreachable until its Funding has committed. Go is asked one more time,
immediately before the entry is handed over, because the read model may lag ([ADR 0008](docs/adr/0008-ach-submission-is-a-sweep.md)).
The row's provider reference is the record that it was sent, and there is no other.

**Settlement**
The provider reports that the entry posted. The pending real leg is posted, and the shadow leg follows once
Go has heard about it — which it does on its own, from the event posting the leg published.

**Return**
The provider reports that the entry will never post (an R-code), or refuses it at submission. The real leg is
cancelled, and the Transaction rolls back once Go has heard about it.

**Clearing**
Moving a settled deposit's money from uncleared cash to cleared cash once the ACH return window has passed:
from the start of the third Federal Reserve business day after the deposit's Transaction completed. It is its
own Go Transaction (`ach_clearing`), originated by a scheduled sweep, with an id derived from the deposit's
([ADR 0003](docs/adr/0003-ach-clearing-as-a-scheduled-sweep.md)). Only deposits clear.

**Progress**
Where an ACH Transaction stands, told as its steps in order: initiation, funding (a withdrawal), submission,
settlement, completion (the Transaction completed) and clearing (a deposit), plus a rollback step while one is
under way or once it has finished. Each step is done, waiting, failed, or skipped because an earlier step
failed. `Services::Ach::Progress` owns the rule; GraphQL exposes it
as `AchTransaction.steps`, and clients render it rather than re-deriving it from raw states.

**Balance**
What an Account holds in one currency, as the read model last saw it: its **posted** amount (settled on the
ledger; negative for an Account that may overdraw, such as `bank`), its **pending outgoing** amount (reserved
by a staged Transfer to leave) and its **pending incoming** amount (reserved to arrive). A pending amount is
posted when its Transfer posts, and released when it is cancelled. Go publishes each Token's balance
(`TokenBalanceRecorded`), and an Account's is the sum over its Wallet's Tokens
([ADR 0005](docs/adr/0005-account-balances-are-projected-from-token-balances.md)). GraphQL exposes it as
`Account.balances`.

_Avoid_: "available balance" — nothing here subtracts pending outgoing from posted, and no rule says it should.

**Role**
What an entity is in this market: `investor`, `borrower` or `issuer`, exactly one each. The role decides which
Accounts onboarding opens for it ([ADR 0006](docs/adr/0006-security-supply-is-ledger-enforced.md)).

_Avoid_: "type" for an entity's role — `type` already names an Account's kind.

**Security**
A claim on one Borrower's debt obligation, offered by an Issuer and bought in fractions by Investors. One
Security stands one-to-one with the obligation behind it, and each names its own Issuer: the platform hosts
many, and nothing treats any entity as the house. Its terms and the ids it was sent to Go under live in
`securities`; no lifecycle state does.

_Avoid_: "loan", "note", "deal" for the Security — those name the Borrower's obligation, which this model does
not record separately. _Avoid_ "share" and "unit": a claim is denominated in the same minor units as the money
that buys it. Go's ledger map is hardcoded to `{"USD": 1}`, so no other denomination is representable, and
counting claims in cents is what makes a pro-rata split exact.

**Supply**
The stock of unsold claims on a Security, held in its `security_supply` Wallet and minted exactly once, for
exactly the offering size, from the Issuer's `issuer_control` Wallet. It is the ledger's own record of what is
left to sell.

**Oversubscription**
Buying more of a Security than remains. Refused by the ledger, not by Ruby: the Supply Wallet permits no debit
past its credits, so the claim leg is refused — normally by Go's accept-time pre-flight, before anything is
written ([ADR 0006](docs/adr/0006-security-supply-is-ledger-enforced.md)). Ruby's "remaining" figure is a
read-model number for the UI and never an authority.

**Subscription**
One Investor's fractional purchase of a Security: a Go Transaction built by the `security_purchase` factory,
with exactly two legs.

_Avoid_: "order" (nothing rests or matches here), "trade", and "investment" for the purchase — that is the
Account type an Investor holds claims in.

**Claim leg**
The Transfer moving claims out of a Security's Supply into the Investor's `investment` Account. It is the root
of a Subscription's DAG and runs first, so an oversubscription is settled before any Investor money moves.

**Money leg**
The Transfer moving the Investor's cleared cash into the Security's **Escrow**. It waits for the claim leg,
and so is never pre-flighted: a shortfall surfaces as a Transaction that initialized and then rolled back,
which `Purchase` learns by resuming it.

**Escrow**
Investor money a Security holds between purchase and **Draw**, in its `security_escrow` Wallet.

**Draw**
Moving a fully-subscribed Security's escrowed money to the Borrower's cleared cash. Its own Go Transaction
(`security_draw`), started by an operator rather than by a Subscription completing — nothing in the platform
reacts to one. Getting that money to a real bank is an ordinary ACH withdrawal from the Borrower's own cleared
cash; the Draw does not cross the bank boundary.

_Avoid_: "disbursement" for the Draw — that word is the Investor side. _Avoid_ "funding": that is already the
ACH withdrawal's word.

**Repayment**
One payment from the Borrower into a Security, split into a principal portion and an interest portion. Its own
Go Transaction (`security_repayment`) with one leg, which collects both as a single amount — the split is a
business fact, recorded on the row where the Allocation reads it. The Borrower gets the cleared cash to pay
from the ordinary way: an ACH deposit that has cleared.

**Interest**
Simple interest on outstanding principal, actual/365, at the Security's rate in basis points, accruing from the
day Go says the money was drawn. Integer arithmetic throughout: a rate carried as a float is exactly what
drifts against the minor units the ledger counts.

**Position**
What one Investor holds in one Security: the sum of their completed Subscriptions, less the principal already
repaid to them. Derived on read, never stored — a stored Position would be a second answer to a question the
Subscriptions and the projections already answer together.

_Avoid_: "holding", "stake" and "allocation" for a Position — *Allocation* is the split of one Repayment.
_Avoid_ "balance": a Balance is what an Account holds, and an Investor's `investment` Account aggregates every
Security they hold.

**Allocation**
Splitting one Repayment across the Positions in its Security, pro rata by outstanding principal, principal and
interest split **separately**. Rounded by largest remainder with ties to the lower entity id, so the parts sum
to exactly the Repayment and the same Repayment always splits the same way
([ADR 0007](docs/adr/0007-disbursement-is-one-transaction-per-holder.md)).

**Disbursement**
One holder's share of one Repayment: a Go Transaction (`security_disbursement`) that retires that much of their
claim and pays them the money. One per holder, never a fan-out across all of them, originated by a scheduled
sweep with an id derived from the Repayment and the Investor.

_Avoid_: "payout" alone — a Disbursement retires a claim as well as paying money, and forgetting the retirement
is how an Investor's Position stops shrinking.

**Retirement**
Returning a repaid claim from the Investor's `investment` Account to the Issuer's `issuer_control` Wallet — the
mint's mirror, and what brings that Wallet's standing negative back towards zero. It is the root of a
Disbursement's DAG, so a holder can never be paid principal they do not hold.

**Stage**
Where a Security stands, told as its phases in order: offering, funded, drawn, repaying, repaid — plus a failed
phase when the Transaction behind one was rejected or rolled back. `Services::Securities::Stage` owns the rule
the way `Services::Ach::Progress` owns an ACH Transaction's; GraphQL exposes it as `Security.stages`, and
clients render it rather than re-deriving it from raw states.

**Money flow**
Where money has gone: every **Movement** the read model has seen complete, and the **Parties** it ran
between, oldest first. `Services::MoneyFlow` derives it and GraphQL exposes it as `moneyFlow`. It is never
stored, for the same reason a Position is not: each business row already says who paid whom and how much,
and its projection says whether and when that completed. It covers money only. Claims being minted, bought
and retired are not money, so they are left out.

**Movement**
One completed Transaction's money, from one Party to another, dated by when Go says the Transaction
completed. Each kind has exactly one entity at one end:
- a deposit runs Bank → entity, and a withdrawal entity → Bank;
- a Subscription runs Investor → Security;
- the Draw runs Security → Borrower;
- a Repayment runs Borrower → Security;
- a Disbursement runs Security → Investor, as two Movements: its principal and its interest.

**Party**
Somewhere money moves between: an entity (by its Role), a Security (its Escrow and Repayment Wallets seen as
one), or the **Bank**. The Bank stands for the world outside the platform, across the ACH boundary.

_Avoid_: "node" and "edge" outside the client. Those are how a graph draws Parties and Movements, not what
they are.

**Business day**
A Federal Reserve business day: not a weekend, not a Fed holiday as the Reserve Banks observe it. Counted in
Eastern time.

**Projection** (read model)
`transaction_projections`, `transfer_projections` and `token_balance_projections`: Ruby's lagging fold of the
published events. Each row
carries the aggregate's last applied `sequence`, and an event at or below it is ignored (the monotonic guard).

_Avoid_: "status" for projected state. The state is Go's; the projection is Ruby's last sight of it.
