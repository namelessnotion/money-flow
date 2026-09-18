# Business backend

Ruby holds the business side of money flow: who the entities are, what they asked for, and a read model of
what the ledger did about it. It originates work in `go/` through its Twirp services and learns the outcome
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
is even staged. Without enough cleared cash it is refused, and the withdrawal is rolled back before anything
reaches the provider ([ADR 0004](docs/adr/0004-ach-withdrawal-funds-before-it-leaves.md)).

**Submission**
Handing the entry to the ACH provider. Once the provider holds it, the staged real leg is confirmed, and it
waits as *pending*.

**Settlement**
The provider reports that the entry posted. The pending real leg is posted, and resuming the Transaction runs
the shadow leg.

**Return**
The provider reports that the entry will never post (an R-code), or refuses it at submission. The real leg is
cancelled, and resuming the Transaction rolls it back.

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

**Business day**
A Federal Reserve business day: not a weekend, not a Fed holiday as the Reserve Banks observe it. Counted in
Eastern time.

**Projection** (read model)
`transaction_projections`, `transfer_projections` and `token_balance_projections`: Ruby's lagging fold of the
published events. Each row
carries the aggregate's last applied `sequence`, and an event at or below it is ignored (the monotonic guard).

_Avoid_: "status" for projected state. The state is Go's; the projection is Ruby's last sight of it.
