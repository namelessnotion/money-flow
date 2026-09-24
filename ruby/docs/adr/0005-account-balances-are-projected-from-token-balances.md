# 5. Account balances are projected from the balances Go publishes per Token

- **Status:** Accepted
- **Date:** 2026-09-18
- **Scope:** `ruby/` (projection, GraphQL) and `go/` (`token.RecordBalances`, `TokenBalanceRecorded`).
- **See also:** [ADR 0001](0001-read-model-consumer-and-follow-on-write-back.md),
  [ADR 0002](0002-projection-consumer-topology-and-halt-policy.md).

## Context

An entity needs to see what each of its Accounts holds. Balances live only in TigerBeetle, one account per
Token, and a Wallet (an Account, to Ruby) owns many Tokens: at least one more for every incoming Transfer.
TigerBeetle is part of Go's write model. Source-Token selection reads it to decide what can fund a Transfer.

Three read paths were considered:

- **A Go query RPC that reads TigerBeetle.** Always current, but it points reads at the write model, while
  this system's read side is Ruby's projections. It would also be Go's first query RPC.
- **Ruby folding the legs on Transfer events into balances.** Needs no Go change, but Ruby would have to
  duplicate TigerBeetle's pending/post/void arithmetic and the Token-to-Wallet mapping. Both are ledger
  knowledge owned by Go, and a copy can drift.
- **Go publishing a balance per Wallet** after each write. Correct, but it costs O(Tokens in the Wallet) per
  write, and a busy Wallet gains Tokens without bound.

## Decision

1. **Go publishes `token.v1.TokenBalanceRecorded` on the Token's own stream** after every TigerBeetle write
   that touched the Token (`transfer.Server.submitBatch` → `token.RecordBalances`). It carries the posted net
   and the pending outgoing and incoming amounts, as TigerBeetle reports them. The cost is O(legs) per write.
2. **The stream sequence orders balances.** `RecordBalances` loads the Token's stream, then reads TigerBeetle,
   then appends at the loaded length. Losing the race means loading and reading again. The last append
   therefore read the ledger after every earlier append, and each earlier append came after its own write,
   so the last balance on a stream reflects every write before it. A balance equal to the last one recorded
   is skipped, so a retried saga step adds nothing.
3. **Recording runs before the saga step's own event.** If it fails, the step is retried: TigerBeetle answers
   `Exists`, and the recording runs again. Publication is at least once, with no gap.
   _Amended 2026-09-24 by [go ADR 0010](../../../go/docs/adr/0010-facts-decided-together-share-a-commit.md):_
   a saga step now records these balances in the same atomic write as its own outcome, so they land with it
   rather than before it. Still at least once, with no gap. Per-Token order (decision 2) is unchanged.
4. **Ruby keeps the highest-sequence balance per Token** in `token_balance_projections`, in one guarded upsert
   (`Consumer::BalanceProjector`). `Account.balances` sums those rows by `wallet_uuid` and currency. One grouped
   query serves a whole page of Accounts (`Sources::AccountBalances`, through `GraphQL::Dataloader`).

## Consequences

- Ruby only adds numbers up; every ledger rule stays in Go.
- A balance lags the ledger by the consumer's delay, like every other projected value.
- A Token that no ledger write has touched since this shipped has no balance row until its next write. That
  includes Tokens funded before the change. A development stack starts clean; a backfill would be a one-off
  `RecordBalances` over existing Tokens.
- Token streams now grow by one event per write that touches them. Tokens are cheap and short-lived (a spent
  source Token is rarely touched again), so this is bounded in practice.
- The consumer now subscribes to `token-events` and acknowledges `TokenMinted` without recording anything.
  An unknown Token event still halts it (ADR 0002).
