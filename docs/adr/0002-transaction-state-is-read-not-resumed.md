# 2. A Transaction's state is read, not resumed — and its width is part of the contract

- **Status:** Accepted
- **Date:** 2026-09-22
- **Scope:** System-wide. Governs the `proto/` published language shared by `go/` and `ruby/`.
- **See also:** [ADR 0001](0001-async-event-driven-saga-and-postgres-to-kafka-publication.md);
  [`go/docs/adr/0006`](../../go/docs/adr/0006-synchronous-dispatch-removed-from-the-rpc-surface.md)
  and [`0007`](../../go/docs/adr/0007-bounded-transaction-width-and-sliced-dispatch.md), the context
  decisions this exists to publish.

## Context

`go/docs/adr/0006` removes synchronous saga dispatch from every RPC handler, and `0007` bounds how
wide a Transaction may be. Both are `go/` decisions about `go/` internals, but each changes something
`ruby/` can observe through `proto/` — and `docs/agents/domain.md` puts decisions about the shared
contract here rather than in either context.

## Decision

1. **`ResumeTransaction` is renamed `GetTransactionState`**, along with its request and response
   messages, and its docblock is rewritten to describe a read.

2. **The `transfer_dependency` docblock states that a Transaction's width is bounded**, and why,
   pointing at `go/internal/transaction` for the number. It previously advertised arbitrary
   many-to-many width with no caveat.

3. **`TransactionRejected.reason`'s comment lists the new rejections** — too many transfers, and a
   leg whose amount no Transfer would accept.

4. **Nothing else in the contract changes.** `StartInitializingTransactionResponse` still carries the
   accept/reject decision and nothing else, exactly as before; what changed is only what a caller may
   infer from an acceptance.

## Why rename rather than fix the comment

A stale comment can be rewritten. The problem is a collision the cutover creates: after it,
`transaction.Server.Resume` really does drive — it is the orchestrator's entire vocabulary — while an
RPC called `ResumeTransaction` no longer does. Two things called *resume* in one system, one of which
does not, at the boundary where `ruby` reads `@go.resume(...)` as a command. `ruby/CONTEXT.md` keeps
a list of words to avoid precisely because this kind of drift is what makes a model stop meaning
anything.

The cost is a regenerate, not a migration: nothing is in production (ADR 0001 says so, and this repo
has done hard cutovers before). The Ruby-side wrapper is renamed to `#state` in the same change, so
neither name outlives the other.

## Consequences

**`ruby/` loses its only way to make a Transaction move.** It could always have been read as one —
"resume this" — and four services used it that way. None does now: `Services::Ach::Submit` is the
single caller, and it calls to be sure of something before handing an ACH entry to a provider, which
is the one thing a lagging projection can never answer. That is what the RPC is for.

**The cap is now something a caller can discover before it builds a shape.** It was previously a
constant in Go's internals contradicted by the contract's own examples.

**`make proto` gained a missing step, found while making this change.** The target never ran
`protoc-gen-twirp_ruby`, so `ruby/gen`'s `*_twirp.rb` kept whatever they last held while every other
generated file regenerated — which is how a rename could have reached Ruby's messages and not its
client. The second pass is guarded on the file declaring a service, since the plugin emits an empty
module for one that does not.

## Not decided here

- Retention, compaction and replication factor for the topics, still open from ADR 0001.
- Whether `proto/` should gain an explicit published-event contract distinct from the internal event
  types. Also still open, and this change did not need it.
