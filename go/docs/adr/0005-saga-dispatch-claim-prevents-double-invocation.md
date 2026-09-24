# 5. A storage-agnostic append claim prevents double-dispatch of a saga side effect

- **Status:** Accepted
- **Date:** 2026-09-19
- **Scope:** `go/` context only.
- **See also:** [ADR 0001](0001-event-triggered-saga-orchestrator.md), whose "resuming an aggregate that has
  already reached its next wait state does nothing" claim this decision found to be false for the
  side-effecting steps, and [ADR 0003](0003-orchestrator-failure-handling.md), whose halt-on-unprocessable-
  message policy is what surfaced the bug this ADR fixes rather than hiding it.
- **Amended 2026-09-24 by** [ADR 0009](0009-operations-are-transfer-legs-not-aggregates.md): Operations are
  now a Transfer's legs, not aggregates, so `operation/server.go` and its terminal-state guard are gone. The
  same tripwire is now `appendSagaStep`'s `transitions` table on the Transfer itself, and it is stricter.
  Mentions below of `operation.Stage/Perform/Cancel/Fail`, and of per-leg writes inside a claimed step,
  describe the code as it stood when this was decided. The claims themselves are unchanged, except that
  `cancelPrepared()` no longer claims `CancellingPreparedTransferStarted`. It waits out any live claim and
  appends its outcome by compare-and-swap (ADR 0009's amendment), so the table row, Round 4's
  `claimedPreparedCancel` recovery, and `TestResume_FinishesAnAbandonedCancelPreparedClaim` below are historical.
- **Amended 2026-09-24 by** [ADR 0011](0011-a-transaction-records-dispatch-intent-before-the-request.md): "The
  Transaction layer needs no claim of its own" is still true of the calls it makes. But recording a call only
  after making it let the record land after a concurrent rollback had concluded. A Transaction now records its
  intent to request a child, against the fold that chose it, before the call.

## Context

A stress test (`cmd/simulate -entities 6 -transactions 6000 -concurrency 150` against 6 Kafka partitions and 6
`cmd/orchestrator` replicas) halted a replica:

```
saga: HALTED on transfer-events[3]@5231: twirp error internal: operation "01a0bac2-...":
already operation.v1.Failed, cannot also become operation.v1.Staged
```

`transfer.Server.runSaga` and its twin `transaction.Server.runSaga` fold an aggregate's current state and
dispatch the next step, but nothing stopped two concurrent callers — the synchronous RPC path in `cmd/server`
and the async orchestrator's `Resume`, or two orchestrator replicas after a Kafka rebalance — from both reading
the same pre-dispatch state and both entering the same side-effecting function (`stage`, `commit`,
`cancelStaged`, `compensate`, `cancelPrepared`) before either appended its own terminal event. Each of those
functions calls an external side effect (a TigerBeetle `submitBatch`, an `operation.Stage/Perform/Cancel/Fail`
call) **before** its own optimistic-concurrency-guarded final append, so the convergence `Resume`'s own doc
comment claims ("a trigger and an RPC drive the Transfer through the same code and converge on the same result
when both do it at once") was true only for the last write, not for the side effect itself.

This is a **pre-existing race**, not something Kafka partition/replica scaling introduced — a single
orchestrator replica racing the synchronous RPC path was equally exposed from the day `cmd/orchestrator` started
running alongside `cmd/server`. Scaling replicas raised a narrow timing window's odds of being hit in one stress
run; it did not create the window. `operation/server.go`'s terminal-state guard did its job exactly as
designed: it refused to write the contradiction and halted loudly (ADR 0003) rather than silently corrupting
the ledger.

## Why not a Postgres advisory lock

`pg_advisory_xact_lock` was the obvious quick fix and was deliberately rejected: it would bake a
Postgres-specific primitive into the domain layer, and `go/CONTEXT.md` already anticipates other event stores
backing `eventstore.Store` in the future ("Other datastores maybe implemented in the future to hold the event
store"). A fix that only works because the current store happens to be Postgres is exactly the kind of coupling
this codebase's domain layer is supposed to stay free of.

## Decision

Use the optimistic-concurrency primitive `eventstore.Store` already provides — `Append`'s
`ExpectedSeq`/`ErrConcurrencyConflict` contract — as a mutual-exclusion claim, generalizing the pattern
`transfer.Server.prepare()` already established: build every write, including the aggregate's own claim on the
next sequence number, and let the CAS decide who proceeds.

### The primitive

```go
// go/internal/eventstore/store.go
func Claim(ctx context.Context, store Store, aggregateType, aggregateID string, expectedSeq int64, marker proto.Message) (won bool, err error)
```

`won=false, err=nil` means a concurrent caller's marker landed first. Storage-agnostic: it's built only on
`Append`, which any backend with per-key conditional append (DynamoDB conditional expressions, S3 conditional
PUT, etcd/Consul CAS) can implement — no SQL, no row locks.

### Transfer side: reuse already-defined, previously-unused events

`proto/transfer/v1/transfer.proto` already defines a `Start*`/`*Started`/`Complete*` triplet for every
transition, flagged unused in ADR 0001's consequences ("leave them defined and unused deliberately, or remove
them"). The `*Started` member of each triplet — a past-tense, `id`-only fact — is exactly the claim marker this
needs, so no proto changes were required on the transfer side:

| Function | Claims | Guards against a concurrent... |
|---|---|---|
| `stage()` | `StagingTransferStarted` | RPC/orchestrator race from `Prepared` |
| `commit()` | `TransferCommittingStarted` | `PostPendingTransfer` retry, or a race with `Prepared`'s auto-commit |
| `cancelStaged()` | `CancellingStagedTransferStarted` | `CancelStagedTransfer` retry, or a race with `commit()` |
| `cancelPrepared()` (statePrepared branch) | `CancellingPreparedTransferStarted` | `CancelAcceptedTransfer` racing `stage()`/`commit()` |

`compensate()` and `cancelPrepared()`'s `stateAccepted` branch need no claim of their own: `compensate` is only
ever entered from inside an already-claimed `stage()`/`commit()` call, and `stateAccepted`'s branch has no
external side effect before its append — the same shape as `prepare()`, which also needs none.

### The claim marker is invisible to `currentState()`'s fold

`currentState()`'s switch has no `case` for the `*Started` events, so it silently ignores them — appending a
claim never moves the aggregate into a new resting state. This is what preserves crash recovery: if the winner
crashes after claiming but before finishing, `currentState()` still reports the pre-claim state, so a later
retry re-enters the same dispatch function, takes a **fresh** claim at the now-incremented `ExpectedSeq`, and
naturally re-runs the already-idempotent side effect (TigerBeetle resubmission of the same deterministic
transfer ID, `operation.Stage`'s "already this fact" convergence). Orphaned markers are harmless breadcrumbs,
never re-read for business logic beyond being CAS anchors. No lease, no owner field, no staleness timeout was
needed.

### Two rounds of getting "a lost claim" wrong before landing on `claimForDispatch`

**Round 1.** The first version had the loser simply `return nil` on a lost claim. That was wrong, and broke
production paths, not just theory: `runSaga`'s own loop, on a `nil` error with the state unchanged, would
immediately loop again, take a **new** claim at the next sequence number, and submit to TigerBeetle a second
time — reintroducing the exact bug one layer up. Worse, `PostPendingTransfer`, `CancelStagedTransfer`, and
`CancelAcceptedTransfer`'s RPC handlers assumed a `nil` return from `commit()`/`cancelStaged()`/`cancelPrepared()`
meant *their own call* had produced the terminal outcome, and read or fabricated a response accordingly —
`CancelAcceptedTransfer` in particular would tell the client "cancelled" even when a concurrent `stage()` had
won instead and nothing was cancelled at all.

**Round 2** (caught in review before shipping, not in production): an `awaitClaimOutcome(ctx, transferID,
preClaimState)` helper polled until folded state moved past `preClaimState`, and a lost claim waited on it
instead of returning early. This still had a gap: **the claim only blocks a caller that read the stream *before*
the marker was written.** A caller that reads *after* the marker lands, but before its winner finishes, sees the
same pre-claim coarse state (`currentState()` ignores markers, by design, for crash recovery) and wins an
independent claim on the *next* sequence slot — then runs concurrently with the still-in-flight first one
against the same Operations. This isn't theoretical: **the marker itself is a normal event on the Transfer's own
stream, so it gets published to `transfer-events` like any other** (root ADR 0001), and the orchestrator calls
`Resume` for every event it sees regardless of type — the marker publication is its own re-entrant trigger.
Concretely: `CancelAcceptedTransfer` wins `CancellingPreparedTransferStarted`; the orchestrator's `Resume` on
that same marker reloads, still sees `statePrepared`, and wins a fresh `StagingTransferStarted` claim while the
cancel is still running — the identical "already Cancelled, cannot also become Staged" contradiction this
decision exists to prevent, just relocated.

**The actual fix: `claimForDispatch`.** Before attempting its own claim, a caller first checks whether the
stream's *trailing* event is already an unresolved marker (any of the four types, not just its own). If so, and
it's younger than `claimStaleAfter` (5s, a `var` so tests can shrink it), the caller polls (20ms interval)
rather than claiming — this is the piece Round 2 was missing. Once the trailing event is no longer a fresh
marker — either because the winner appended its terminal event, or because the marker aged past
`claimStaleAfter` (the winner crashed) — the caller re-checks `currentState()` against `preClaimState`: if it
moved on, there's nothing left to claim (`won=false, err=nil`, and the caller returns `nil` immediately, no
further polling needed); if it's still `preClaimState` and no fresh marker remains, the caller attempts its own
`eventstore.Claim` and loops back through the same check on a lost CAS. This makes "returned nil" an
unconditional guarantee that the transition happened, by this call or another — every caller (`runSaga`'s loop
and the three RPC handlers) can trust it without re-deriving the outcome. `runSaga` additionally compares
folded state before and after each dispatch as a defensive backstop (belt-and-suspenders, not load-bearing
given the guarantee above).

Staleness, not a lease or heartbeat, is what makes this safe for crash recovery without reintroducing Round
2's gap: a *fresh* marker always wins the gate (mandatory wait, no reclaim), so two live callers can never
both proceed; only a marker old enough to plausibly mean "the winner crashed" is treated as reclaimable.
`TestStage_DoesNotRaceAFreshInFlightClaimFromADifferentTransition` (`go/internal/transfer/dispatch_claim_test.go`)
seeds an in-flight marker directly and asserts a concurrent `stage()` call blocks — never touching TigerBeetle —
until the marker's real resolution lands; it fails immediately (no error, no wait, one submission where zero
was expected) if the marker gate is removed, which is how this was verified against the Round 2 code rather
than only reasoned about.

### Round 3: three more gaps found in external review, before this shipped

Round 2's design — check the trailing event for a marker, wait if fresh (`time.Since`), reclaim if stale — had
three further problems, all caught in review and fixed before this reached production. Each is verified with a
test that fails against the pre-fix code (checked by temporarily reverting just that piece) and passes with it.

**1. Only the literal trailing event was checked, and other RPCs write to the same stream.**
`ConfirmStagedTransfer`, `CancelStagedTransfer`, and `PostPendingTransfer` each record their own `*Rejected`
response onto the Transfer's own stream when called against a state that doesn't match what they expected —
none of which advances `currentState()`'s fold. If one of those lands *after* a still-live marker (a client
calling `ConfirmStagedTransfer` while `stage()` is mid-flight and the Transfer is still `Prepared`, say), the
rejection becomes the new trailing event and hides the marker underneath it — a fresh reader sees no marker at
the tail, concludes nothing is claimed, and claims the next slot while the original is still running. Fixed by
`liveMarker`, which scans backward from the end and stops at the first event that either (a) is a claim marker
— that's the live one — or (b) advances `currentState()`'s fold (`stateByEventType`, now a single shared map so
`currentState()` and `liveMarker` can't drift apart on which events count as real transitions). A `*Rejected`
response matches neither, so the scan skips straight through it.
(`TestStage_SeesALiveClaimBehindAConfirmStagedTransferRejection`.)

**2. `claimStaleAfter` alone doesn't stop a merely slow winner from being reclaimed out from under itself.**
Treating a marker as abandoned after 5 seconds is a guess about crashes, not a guarantee the original caller is
gone — and this codebase has already measured multi-second Postgres write latency under sustained load (see the
session notes this ADR grew out of), so a step legitimately taking that long is plausible, not hypothetical. The
original claimant never checked whether it still held the claim before its TigerBeetle submission or its final
append, so a reclaim mid-flight let both callers proceed. Fixed with `stillHoldsClaim`/`requireClaim`: every
externally visible action in `stage()`/`commit()`/`cancelStaged()`/`cancelPrepared()`/`compensate()` — before
`submitBatch`, before the `operation.*` writes, before the terminal `appendSagaStep` — re-checks that its own
`claimedSeq` (the sequence number its marker landed at) is still `liveMarker`'s answer, via the shared
`errClaimSuperseded` sentinel, and stops (returns `nil`) the instant it isn't.
(`TestRequireClaim_DetectsSupersessionAfterAStaleReclaim` checks `stillHoldsClaim`/`requireClaim` directly
after a provoked stale reclaim.)

**3. The staleness comparison mixed two different clocks.** `time.Since(marker.OccurredAt)` compared
`occurred_at` — `TIMESTAMPTZ NOT NULL DEFAULT now()`, set server-side by Postgres
(`go/db/migrations/00001_create_events.up.sql`) — against the Go process's own clock. If the two drift (a
real possibility across a container/host boundary), a database clock running even a few seconds behind makes
every marker look instantly stale on arrival, permanently defeating Round 3.2's fix; a database clock running
ahead can make a marker never go stale at all. `MemoryStore` never had this problem (it stamps `OccurredAt`
with the same Go clock it would be compared against), so no test running purely against `MemoryStore` could
have caught it. Fixed by adding `Now(ctx) (time.Time, error)` to the `Store` interface — `PostgresStore.Now`
runs `SELECT now()` on the same connection, `MemoryStore.Now` returns `time.Now().UTC()` — so `claimForDispatch`
compares an event's age using the same store's clock it was stamped with, never the Go process's own.
(`TestClaimForDispatch_UsesTheStoresClockNotTheHostClock` wraps `MemoryStore` with a clock skewed an hour
behind and confirms a marker real time has already aged past `claimStaleAfter` is still treated as fresh.)

### Round 4: a stale claim may only be taken over by the same transition

`requireClaim` checks between steps, not during them: the `forEachOperation` loop over every leg and the
`submitBatch` round trip run unchecked. A same-transition takeover inside that window is harmless — TigerBeetle
answers `Exists` for the same deterministic transfer IDs and `operation.*` converges on the same fact. A
*different* transition is not: a slow `cancelPrepared()` still cancelling later legs, reclaimed by a `stage()`
that stages the same Operations, lands "already Cancelled, cannot also become Staged"; a `cancelStaged()` void
racing a `commit()` post ends with `compensate()` failing Operations the cancel is cancelling. The same holds
for a genuine crash — the abandoned step may be half-applied, and only that step converges over it.

So `claimForDispatch` takes over a stale marker only when it is the caller's own marker type. A different type
returns `errAbandonedClaim`, a twirp `FailedPrecondition` (`sagaStepError` keeps that code through the
`CancelAcceptedTransfer`/`CancelStagedTransfer`/`PostPendingTransfer` handlers, rather than re-wrapping it as
`Internal`). That leaves recovery to the claimed transition itself:

- A crashed `CancelAcceptedTransfer` on a Prepared Transfer is finished by the orchestrator: the marker's own
  publication triggers `Resume`, and `runSaga` checks `claimedPreparedCancel` before its usual Prepared dispatch,
  running `cancelPrepared()` with the marker's reason instead of `stage()`/`commit()`. Without this the
  orchestrator's own dispatch would hit the refusal and halt its partition (ADR 0003).
- A crashed `stage()`/`commit()` from Prepared is re-dispatched by `runSaga` as before — same marker type.
- A crashed `cancelStaged()` or `commit()` from Staged/Pending is not auto-resumed (`runSaga` parks there by
  design); it waits for the client to retry that same RPC. The other RPC is refused with `FailedPrecondition`
  meanwhile.

(`TestStage_RefusesStaleTakeoverOfADifferentTransition`, `TestResume_FinishesAnAbandonedCancelPreparedClaim`,
`TestCancelAcceptedTransfer_RefusesOverAnAbandonedStageClaim`.)

### A second, unrelated bug produced the identical symptom: `submitBatch`'s rejection contract

After deploying the claim fix, the exact same halt (`"already operation.v1.Failed, cannot also become
operation.v1.Staged"`) recurred under a fresh stress run, on a **single, uncontested** claim — no concurrency
involved at all. `submitBatch` (`saga.go`) returned `onReject`'s result directly:

```go
for i, r := range results {
    if r.Result != ledger.TransferResultOK && r.Result != ledger.TransferResultExists {
        return onReject(i, r.Result)
    }
}
```

`stage()`/`commit()` pass `compensate()` as `onReject`. `compensate()` **legitimately returns `nil`** once it
has appended `TransferFailed` — that's success, not "nothing happened." But `stage()`/`commit()`'s own caller
code only stopped on a *non-nil* error:

```go
if err := s.submitBatch(ctx, batch, func(...) error { return s.compensate(...) }); err != nil {
    return err
}
if err := forEachOperation(legs, func(operationID string) error {
    _, err := operation.Stage(ctx, s.store, operationID) // reached even after compensate() succeeded
    return err
}); err != nil { ... }
```

Any ordinary TigerBeetle rejection (insufficient balance under real contention, no race required) made
`compensate()` mark every Operation `Failed` and append `TransferFailed` — correctly — and then `stage()`/
`commit()` fell straight through and tried to `Stage`/`Perform` the same, now-terminal Operations, hitting
`operation/server.go`'s guard every time. This is older than the claim work and **not concurrency-related** —
the existing tests (`TestStage_TigerBeetleRejectionRoutesToFailed`,
`TestCommit_TigerBeetleRejectionRoutesToFailedNotCancelled`) never caught it because `RequestTransfer` drives
`runSaga` through `logSagaError`, which *logs* a saga failure rather than returning it, and `compensate()` had
already durably written `TransferFailed` before the erroneous fall-through — so the Transfer's own final state
was still correct even while the Operation-level contradiction fired and got silently swallowed.

Fixed by making rejection distinguishable from "no rejection" regardless of how `onReject` resolved it:

```go
var errBatchRejected = errors.New("transfer: batch rejected")
...
if err := onReject(i, r.Result); err != nil {
    return err
}
return errBatchRejected
```

`stage()`/`commit()` now treat `errBatchRejected` as "stop, cleanly" — the same outcome as before, just reached
correctly instead of by accident:

```go
switch err := s.submitBatch(...); {
case err == nil:
case errors.Is(err, errBatchRejected):
    return nil // compensate() already recorded TransferFailed
default:
    return err
}
```

`cancelStaged()`'s own `onReject` always returns a non-nil `twirp.InternalError`, so it was never exposed to
this bug and needed no change. Regression tests
(`TestStage_TigerBeetleRejectionReturnsNilNotAContradiction`,
`TestCommit_TigerBeetleRejectionReturnsNilNotAContradiction`, `go/internal/transfer/saga_failure_test.go`)
call `stage()`/`commit()` directly — bypassing `logSagaError`'s swallowing — and assert `nil`, closing the blind
spot the existing RequestTransfer-based tests had.

### The Transaction layer needs no claim of its own

`transaction.Server`'s `requestChildTransfer` and `rollbackChild` call into `transfer.Server` (`RequestTransfer`,
`RequestReversal`, `CancelStagedTransfer`, `CancelAcceptedTransfer`) before appending their own bookkeeping
event, the same shape as the transfer-side bug. They were **not** given a claim, and this was verified
empirically (`TestOrchestrator_ConcurrentDispatchOfTheSameReadyChildConverges`,
`go/internal/saga/orchestration_test.go`), not just argued: two concurrent drivers dispatching the same ready
child are safe by composition of two properties that already existed —

1. Every RPC `requestChildTransfer`/`rollbackChild` calls into is now idempotent under concurrent identical
   calls (this decision's transfer-side fix), so two concurrent callers always get the same accept/reject
   outcome for the same child.
2. `transaction.Server.appendSagaStep` dedupes by `(event type, transfer_id)` across the *whole* stream, not
   just its tail (unlike transfer's own `appendSagaStep`, which only checks the tail) — so two callers recording
   the same child's outcome converge without a race window to close.

The property that made the transfer-side functions dangerous — a genuinely divergent external outcome
(TigerBeetle accepting one submission and rejecting an identical concurrent one) reaching two different
callers — doesn't exist one layer up, because by the time the transaction layer's callers race, the callee has
already converged.

## Consequences

**Every `eventstore.Store` implementation must support single-stream `Append`/`ErrConcurrencyConflict`
faithfully** — already a requirement (`PostgresStore` and `MemoryStore` both do), now load-bearing for
correctness under concurrency, not only for idempotent replay.

**`Store` gained a `Now(ctx) (time.Time, error)` method** (Round 3.3), which every implementation must answer
on the same clock it stamps `Event.OccurredAt` with. This is a real, if small, new obligation on any future
backend — but a lighter one than it sounds: any store with its own server-side "current time" (a database's
`now()`, a KV store's server timestamp) can satisfy it directly; only a backend with no such notion at all
would need to fall back to the Go process's clock, in which case it's no worse off than before this ADR.

**Claim markers add event-log volume, and `requireClaim` adds Postgres round trips.** Every `stage()`/`commit()`/
`cancelStaged()`/`cancelPrepared()` call now writes one extra event, whether or not it races anything, and
Round 3.2's `requireClaim` adds up to three extra `Load` calls per dispatch (before the TigerBeetle submission,
before the `operation.*` writes, before the terminal append) — real cost on a system this session's own load
testing already found to be Postgres-write-bound, accepted because the alternative is a caller silently
proceeding after being superseded. Accepted: the markers are `id`-only messages, and an out-of-band lock was
rejected on portability grounds above.

**A caller that loses a claim can now wait up to `claimStaleAfter` (5s) before proceeding**, rather than
erroring or racing immediately. This is a real, if rare, added latency on the losing path — but unlike Round
2's `awaitClaimOutcome`, it never surfaces as an error on its own: once a marker goes stale, the waiting caller
simply proceeds to claim it itself, self-healing in the same call rather than requiring a fresh retry from
outside. The 5s figure trades recovery latency against how long two legitimately concurrent callers might wait
on each other under real load; revisit with production latency data before this cutover completes.

**Not decided here:** a formal choice of `claimStaleAfter`/`claimPollInterval` beyond "generous relative to a
TigerBeetle round trip, tight relative to `cmd/simulate`'s 30s RPC timeout."
