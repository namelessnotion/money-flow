# 7. A Transaction's width is capped, and its dispatch is sliced

- **Status:** Accepted
- **Date:** 2026-09-22
- **Scope:** `go/` context. The cap is part of the published language; see
  [root ADR 0002](../../../docs/adr/0002-transaction-state-is-read-not-resumed.md).
- **See also:** [ADR 0006](0006-synchronous-dispatch-removed-from-the-rpc-surface.md), which lands
  alongside this; [ADR 0002](0002-asynchronous-rollback-and-reversal-reconciliation.md) on what a
  rollback costs; [ADR 0004](0004-transaction-accept-time-funding-preflight.md) on the pre-flight's
  known limitation; [`ruby/docs/adr/0007`](../../../ruby/docs/adr/0007-disbursement-is-one-transaction-per-holder.md),
  which this partly supersedes and which is amended accordingly.

## Context

`validateDAG` rejected an empty transfer set, a dangling reference and a cycle. It never looked at
how many transfers a Transaction held. Nothing else did either: `dispatchReady` looped over every
ready child, `ledger.CreateTransfers` passed the caller's whole slice through unchunked.

The only thing preventing a two-hundred-leg Transaction was `ruby/docs/adr/0007`, which decided that
a Disbursement is one Transaction per holder and wrote down three reasons for it. All three live in
Ruby class comments. Go's published language says the opposite: the `transfer_dependency` docblock
advertises arbitrary many-to-many width, so any caller — a second Ruby capability, a different
context — could send the shape ADR 0007 forbids and Go would accept it. The invariant had no owner.

ADR 0006 changes the arithmetic, and honestly weakens one of those three reasons. With
`RequestTransfer` asynchronous, `dispatchReady` no longer runs N Transfer sagas, so *"two hundred
Transfer sagas in one HTTP call… a retried timeout re-entering a call Go is still executing"* stops
being true. That was ADR 0007's loudest argument and it is gone.

## Decision

1. **`maxTransfersPerTransaction = 64`, enforced in `validateDAG`**, producing `TransactionRejected`
   before anything is written, checked before any graph walk and before `wouldAcceptReadyChildren`
   so an over-wide request is refused at its cheapest.
2. **`maxDispatchPerStep = 8`.** `dispatchReady` dispatches at most that many ready children and
   then ends the saga run rather than looping. `rollbackNext` does the same.
3. **`validateDAG` also rejects a leg whose amount no Transfer would accept**, by calling
   `money.Validate` rather than restating the rule.

## Why both, given one of ADR 0007's reasons is gone

Its other two survive, and the cutover adds a fourth.

**Rollback blast radius — what the cap bounds.** The Transaction is the unit of rollback, so an
N-child Transaction failing is N reversals, each a Transfer running its own saga, and
`TransactionRollbackFailed` is a terminal that requires a person (ADR 0002). A unit of rollback whose
failure leaves two hundred committed movements to reconcile by hand is not a unit anyone can reason
about. Unchanged by the cutover; it is a property of the model, not of the driving.

**The pre-flight's window widens.** ADR 0004 is explicit that N ready siblings drawing on one wallet
are each checked against the same unconsumed snapshot, and that this is a check rather than a
reservation. The gap between the check and the real dispatch was microseconds; it is now a CDC round
trip. Neither instrument fixes this. The cap bounds the damage.

**Trigger amplification — what the slice bounds.** Every event on a Transaction's stream is a CDC
message, and every message re-runs the whole fold. The synchronous path paid its O(N²) once; the
async path pays it per published event. Per ADR 0003 a handler that keeps failing halts the consumer
for every other aggregate sharing it, so one pathological Transaction becomes a system-wide stall.

**TigerBeetle's batch ceiling is unguarded.** `ledger.CreateTransfers` passes the caller's whole
slice through with no chunking, and a linked chain cannot span batches. `lookupBatchMax = 8189`
exists, but only for reads. The cap keeps the ceiling unreachable rather than discovered.

> **Wrong, found 2026-09-23 by [ADR 0008](0008-transfer-legs-span-ledger-batches.md).** The cap counts
> Transfers. The ceiling counts legs, and one Transfer drawing on a Wallet of many small Tokens has one leg
> per Token. A 276-leg security draw hit it and halted the orchestrator. The ceiling is now guarded where it
> lives: `ledger.BatchMax` (253 under `--development`), with wide Transfers reserving before they post. This
> reason for the cap no longer stands. The other three do.

## Why the slice needs no cursor

**Every slice that reports more work has appended at least one event.** `readyToRun` returns only
children absent from `touched`, which is folded from any per-child event; `appendSagaStep` dedupes
on `(type, transfer_id)`, so a ready child's append cannot be deduped. Each dispatched child produces
exactly one of `TransferRequested`/`Gated`/`FailedWithinTransaction`. Every append is published and
keyed by `aggregate_id`, so slice *k* schedules slice *k+1* with its own writes, and the untouched
set shrinks by at least one each time. The stream is the cursor — the same idiom the rest of this
aggregate uses, where state is a fold and never a stored position.

That invariant is load-bearing enough to be a test rather than a comment
(`TestDispatchReady_EverySliceAppendsAtLeastOneEvent`).

Three things it does not say, all of which belong here:

- **It bounds one unit of work, not total work.** N children still cost N dispatches; they cost them
  in ceil(N/K) separately-retryable runs instead of one unbounded fold. Trigger fan-out makes total
  folds O(N·K), not O(N/K). With N ≤ 64 and K = 8 that is fine, but nothing here makes a wide
  Transaction cheaper.
- **K is per run, not in flight.** ADR 0001 records that a transfer-topic and a transaction-topic
  message can be handled concurrently and both append here, so two runs may take disjoint slices.
  Safe for the reasons ADR 0005 gives; the absolute ceiling stays `maxTransfersPerTransaction`.
- **Which children a slice draws is unspecified**, because `readyToRun` ranges a map. Nothing depends
  on it: the DAG constrains a child only by its parents, and every ready child is unconstrained.

## Rollback filters before it slices, and the reason is not symmetry

`rollbackNext` skips a child already recorded as `childRollbackFailed` without appending anything —
it is never retried automatically. Slice the ready list directly and a slice can consist entirely of
those: the call reports no progress, falls through to `blocked`, and records
`TransactionRollbackFailed` **while children that could still have been reversed never were**. Money
left out rather than put back, and a Transaction telling an operator it is beyond help when it was
only unfinished.

Filtering first means `blocked` is reached only once nothing rollbackable remains. Forward dispatch
needs no equivalent, because every child `readyToRun` returns appends exactly once.

This was got wrong first time round, in the obvious way: the failure was predicted as a *stall*, and
the test written for a stall passed against the broken code. The draw is random, so the real fault
shows probabilistically; `TestRollbackNext_NeverReportsFailedWhileAChildCouldStillBeReversed`
repeats over fresh state, and is deterministic against correct code because the stuck children are
then never candidates at all.

## Why 64 and 8

Neither is derived from anything. The widest shape this system builds is two legs, so 64 is thirty
times the headroom in use and the numbers exist to catch a caller that has lost track of what it is
assembling, not to shape a design. 8 is comfortably above every root count in use, so no shape in the
repo is sliced at all today and the mechanism is exercised only by its own tests.

They are constants in one place each. If a legitimate shape ever approaches either, raise it and
record why — that is a smaller decision than this one.

## Consequences

**`ruby/docs/adr/0007` is partly superseded and has been amended**, rather than left to read as
though all three of its reasons still stand. Per-holder Disbursements remain right, on the two
reasons that survive.

**A zero-amount leg is now a rejection rather than a stall.** `money.Validate` refuses a nil `Money`,
a missing currency and zero minor units; such a leg used to be refused at dispatch as a transport
error that reached nobody, leaving the Transaction in `Started` with nothing on its stream to explain
why it would never move again. `ruby/docs/adr/0006` asked for exactly this, and Ruby's three
workarounds stay as belt-and-braces.

**A DAG between 9 and 64 children wide now takes several trigger round trips to dispatch.** Nothing
builds one today.
