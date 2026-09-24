# Tokenized Transaction System

This is a tokenized transaction system.

Event Sourced, CQRS, and DDD are used to implement the system.

Event store is stored in a single Postgres table, as an append-only log. Other datastores maybe implemented in the future to hold the event store, but for now, Postgres is the only supported event store.

Domain commands and events are defined in the `proto/` directory using twirp RPC to implement the services.

## Glossary

**Transfer**
Moves an amount from a source Wallet to a destination Wallet by minting new Token(s) in the destination and
debiting existing (or, for `mint_source`, freshly minted) Token(s) in the source. It is an aggregate: its own
stream is the whole record of what it did.

**Leg**
One source Token → destination Token movement inside a Transfer (`TransferLeg`), carried out as exactly one
TigerBeetle transfer under the leg's `ledger_transfer_id`. A leg is an entity inside its Transfer, recorded on
`TransferPrepared`, and it reaches the Transfer's own outcome together with every other leg
([ADR 0009](docs/adr/0009-operations-are-transfer-legs-not-aggregates.md)).
_Avoid_: Operation, a DEBIT or CREDIT Operation. Those were aggregates of their own until ADR 0009 folded them
into the Transfer.
