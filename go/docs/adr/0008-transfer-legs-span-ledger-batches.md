# 8. A Transfer wider than one ledger batch reserves before it posts

- **Status:** Accepted
- **Date:** 2026-09-23
- **Scope:** `go/` context only. Nothing in the published language changes.
- **See also:** [ADR 0007](0007-bounded-transaction-width-and-sliced-dispatch.md), which assumed this ceiling was
  out of reach and was wrong; [ADR 0003](0003-orchestrator-failure-handling.md), whose halt this incident
  triggered; [ADR 0005](0005-saga-dispatch-claim-prevents-double-invocation.md), whose claims make the retries
  here safe.

## Context

On 2026-09-23 `ruby/bin/simulate_lending` (seed 42, 40 investors) drew a security: Transaction
`bf65e369-6611-4949-8d4f-69ec1762b4dc` moved a `security_escrow` Wallet holding 276 Tokens, one per
Subscription, to the Borrower. Its one Transfer, `d0803d1c-090d-4565-ac2b-eb5e76564a85`, was prepared with 276
legs. `commit()` submitted them as one linked batch. `tigerbeetle-go` refused the request with `ErrTooMuchData`.
The orchestrator retried three times, halted on `transfer-events[3]@88902`, and exited. After a restart it
halted again on the same message. The partition was wedged on a message no retry could get past.

ADR 0007 had written this ceiling down as "unguarded" and concluded that the 64-Transfer cap "keeps the
ceiling unreachable". It didn't, because the cap counts Transfers and the ceiling counts **legs**. One Transfer
drawing on a Wallet that holds many small Tokens has one leg per Token, and FIFO selection
(`selectSourceTokens`) takes as many Tokens as the amount needs. Nothing bounds that number. An escrow Wallet
that collects one Token per Subscription is exactly the shape that reaches it.

### What the actual limit is

276 × 128 B is 35 KiB, far under TigerBeetle's 1 MiB message, so the limit was not the obvious one. Measured
against a real 0.17.9 replica (a throwaway container, and since then pinned by
`TestRealClient_BatchMaxLinkedTransfersFitOneRequest`):

| request | `--development` replica |
|---|---|
| `CreateTransfers`, unlinked pending | 253 |
| `CreateTransfers`, one linked pending chain | 253 |
| `CreateTransfers`, one linked post-pending chain | 253 |
| `CreateAccounts` | refused at 300; same 128 B event, so 253 |
| `LookupAccounts` ids | 2031 |

The culprit is `--development` (`docker/tigerbeetle/entrypoint.sh`). As well as allowing non-Direct-IO
storage, it "uses smaller cache sizes and batch size by default": it shrinks the request to 32 KiB. After the
256-byte header and the multi-batch trailer, that holds 253 128-byte events, or 2031 16-byte lookup ids.
Linking is not a factor, and neither is post-pending: all three shapes stop at the same count. A production
replica's 1 MiB request holds 8189. The client checks the size itself before sending, so a refused request
applied nothing.

Reads had the same gap. `Balances` chunked `LookupAccounts` at 8189, the 1 MiB reply limit, so a Wallet holding
more than 2031 Tokens would have failed selection under `--development`.

## Decision

1. **`ledger.BatchMax = 253`, enforced by both clients.** It is the per-call ceiling under the smallest request
   any replica we run uses. `CreateTransfers` and `CreateAccounts` refuse a larger batch before sending it. So
   does `FakeClient`, so a saga that would overflow fails its unit tests instead of the orchestrator.
   `lookupBatchMax` becomes 2031 by the same reasoning. Both are conservative in production. That is
   deliberate: every replica in a cluster shares one batch size, and the code has to run against both.
2. **`ledger.ErrInvalidRequest` names a refusal no retry can change.** That covers an oversized batch, a batch
   ending mid-chain, a malformed id, an unknown currency, and TigerBeetle's own `ErrTooMuchData`. In every case
   nothing was applied. Transport failures (eviction, shutdown) stay plain errors.
3. **A saga step submits one linked chain of at most `BatchMax` legs per request, in leg order, and stops at the
   first chain refused.** Leg order is the order `TransferPrepared` recorded, so every retry cuts the same
   chains. A Transfer of `BatchMax` legs or fewer is one chain: same ids, same kinds, same single round trip as
   before.
4. **A Transfer wider than one chain, committed from Prepared, reserves every leg before it posts any.** It no
   longer submits `Regular` transfers. It submits the pending reservations `stage()` already uses (id = DEBIT
   Operation id), then post-pendings (`detid(op:post)`). The staged path needed nothing new: it already
   reserves, then posts or voids, and each of those steps is now chained.
5. **A refusal fails the Transfer if and only if nothing has posted.** An `ErrInvalidRequest` counts as a
   refusal, which is what stops a deterministic ledger error from halting a partition (ADR 0003). A refused
   reservation posts nothing, so `compensate()` records Failed and the owning Transaction rolls back as for any
   refused leg. If a post is refused after an earlier chain has posted, part of the money has moved. That state
   is neither Committed nor Failed, so the step returns an error. The orchestrator halts on it, and a person
   reconciles.

## Why reserve-then-post rather than the alternatives

**Why not bound legs per Transfer at accept.** It would turn this halt into a rejection. The escrow draw is a
legitimate business operation, and refusing it because of how many Subscriptions funded the security leaks a
storage detail into the domain. The Borrower would not be funded, for a reason nobody on the business side can
act on. Consolidating Tokens first would need a new domain operation that nothing asks for yet.

**Why not split in Ruby into several Transfers.** Ruby would have to know TigerBeetle's request size. The
Transaction would have to carry the pieces, and it is capped at 64 Transfers. And the pieces would only be as
atomic as a Transaction rollback, which is compensating reversals and not all-or-nothing. The Transfer is the
unit that is meant to be atomic, so atomicity belongs to it.

**Why not chunk `Regular` transfers.** A refusal in chain *k* would leave chains 0..k-1 posted, and undoing
them means inverse transfers the model has no name for. A reservation moves no money, so a partial reservation
is harmless. And a post against a live reservation cannot be refused for balance: the reservation already holds
it.

**Why posting stops at the first refused chain, and why that makes a partial post practically unreachable.**
A post can only be refused if its reservation has gone, voided or expired. Voids come only from
`cancelStaged`, which ADR 0005's claims keep off a Transfer that is committing. Expiry is 10 days, and chains
are reserved in the same order they are posted, so chain 0 lapses first. If chain 0 posts, every later chain
was reserved after it and is still live. A refusal therefore surfaces at chain 0, with nothing posted, and
fails the Transfer cleanly. Decision 5's halt is the backstop for the case this argument rules out, not a path
expected to run.

## Consequences

**A reservation refused part-way leaves the earlier chains reserved until their timeout (10 days).** No money
moves. But those Tokens' capacity is locked, and the read side shows it as pending. They are deliberately not
voided on the way to Failed. The void would have to happen before `TransferFailed` is durable. A crash in that
window resumes into `commit()` again, and if the refused chain now succeeds, a post would run against some
voided and some live reservations. That is the partial post decision 5 exists to avoid, now caused by our own
cleanup. The lock is bounded, and it needs a concurrent spend to race the funding checks at both accept and
prepare. Releasing it safely needs a durable "released" fact the saga can check before re-reserving. That
follow-up is not taken here. The same was already true of a refused post on the staged path.

**A wide immediate commit costs two round trips per chain instead of one**: 276 legs is four requests where it
would have been one. Only Transfers over `BatchMax` legs pay it.

**`BatchMax` must not change while a wide Transfer is between reserving and committing.** The chain boundaries,
and whether a Transfer is "wide" at all, derive from it. A narrow Transfer posts `Regular` under the DEBIT
Operation id, and a wide one reserves under that same id. Moving the constant across an in-flight Transfer
would re-cut its chains, or switch its kind so TigerBeetle answers `ExistsWithDifferentFields`. Raise it only
with nothing Prepared-and-claimed.

**Recovery of the 2026-09-23 halt needs no offset change.** `ErrTooMuchData` is refused client-side, so none of
the six attempts reached TigerBeetle. The Transfer's stream is `TransferPrepared` followed only by stale
`TransferCommittingStarted` markers. Once the orchestrator runs this code, redelivering `@88902` re-claims the
stale marker (same marker type, ADR 0005), reserves and posts in two chains, and records `TransferCommitted`.
The runbook is in [`docs/saga-orchestrator.md`](../../../docs/saga-orchestrator.md#recovering-from-a-halt).

**Still unbounded: the width of `TransferPrepared` itself.** It carries every leg, about 165 bytes each (276
legs is 45,601 bytes). One Operation stream per leg is created in the same `AppendAtomic`. Both scale with the
Wallet's Token count, and the event rides CDC to Kafka, whose default message ceiling is 1 MB, somewhere around
6,000 legs. When a real shape approaches that, the answer is a cap at accept time with a named rejection, or
Token consolidation. That is a decision about the domain, not the ledger, and it is not taken here.
