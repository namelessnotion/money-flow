# Business backend

Ruby holds the business side of money flow: who the entities are, what they asked for, and a read model of
what the ledger did about it. It originates work in `go/` through its Twirp services and learns the outcome
from `transfer-events` / `transaction-events` ([ADR 0001](docs/adr/0001-read-model-consumer-and-follow-on-write-back.md)).
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
deposit, `cleared_cash → bank_control` on a withdrawal. It depends on the real leg, and runs only once the
real leg has posted.

**Submission**
Handing the entry to the ACH provider. Once the provider holds it, the staged real leg is confirmed, and it
waits as *pending*.

**Settlement**
The provider reports that the entry posted. The pending real leg is posted, and resuming the Transaction runs
the shadow leg.

**Return**
The provider reports that the entry will never post (an R-code), or refuses it at submission. The real leg is
cancelled, and resuming the Transaction rolls it back.

**Projection** (read model)
`transaction_projections` and `transfer_projections`: Ruby's lagging fold of the published events. Each row
carries the aggregate's last applied `sequence`, and an event at or below it is ignored (the monotonic guard).

_Avoid_: "status" for projected state. The state is Go's; the projection is Ruby's last sight of it.
