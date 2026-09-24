# 6. Synchronous saga dispatch is removed from the RPC surface

- **Status:** Accepted
- **Date:** 2026-09-22
- **Scope:** `go/` context. Its consequences reach `ruby/` through the response contract; those are
  recorded in [`ruby/docs/adr/0008`](../../../ruby/docs/adr/0008-ach-submission-is-a-sweep.md).
- **See also:** [root ADR 0001](../../../docs/adr/0001-async-event-driven-saga-and-postgres-to-kafka-publication.md),
  which anticipated this cutover; [ADR 0001](0001-event-triggered-saga-orchestrator.md), whose
  trigger semantics this depends on; [ADR 0003](0003-orchestrator-failure-handling.md), whose staging
  precondition this discharges; [ADR 0004](0004-transaction-accept-time-funding-preflight.md), whose
  closing "Not decided here" this resolves; [ADR 0007](0007-bounded-transaction-width-and-sliced-dispatch.md),
  which lands alongside it.
- **Amended 2026-09-24 by** [ADR 0012](0012-a-ready-child-is-always-requested.md): `StartProcessingTransfer` and
  gated children are removed, so the paragraph below explaining why it could still dispatch is historical.

## Context

Every RPC handler in `transaction` and `transfer` recorded its decision and then ran the saga
in process, on the caller's goroutine, before building a response that could not report any of it.
`StartInitializingTransaction` dispatched the whole DAG; `RequestTransfer` prepared, staged or
committed. The response was always built from the accept/reject decision alone, which is why four
Ruby services followed every origination with a second call to find out what had actually happened.

Root ADR 0001 planned for this to end. ADR 0003 named the precondition — *"staging the eventual
cutover needs the consumer deployed and observed before the synchronous dispatch is removed"* — and
`cmd/orchestrator` has been running beside `cmd/server` since, driving the same sagas from the same
`Resume` entry points. ADR 0004 closed by listing this removal as the one thing it would not decide.

## Decision

1. **No RPC handler runs a saga.** The eight `runSaga` calls are gone: both paths of
   `StartInitializingTransaction`, `StartProcessingTransfer`, `ResumeTransaction` and
   `StartTransactionRollback`; both paths of `RequestTransfer` and `RequestReversal`. `runSaga` is
   reachable only through `Resume`, and `Resume` is called only by `cmd/orchestrator` — and, out of
   band, by `cmd/resume`.
2. **The accept/reject decision stays synchronous, and stays where it was.** DAG validation and
   `wouldAcceptReadyChildren` both run before anything is written and both still answer the caller.
   Nothing about the response contract changes; what changes is that a successful response now means
   `INITIALIZED` rather than "whatever the saga reached".
3. **`StartInitializingTransaction` does not append `TransactionStarted`.** That transition belongs
   to `runSaga`'s `stateInitialized` branch and stays there.
4. **`ResumeTransaction` becomes a read, and is renamed `GetTransactionState`.** It folds the stream
   and answers; it advances nothing. Recorded in the root ADR, since it is published language.
5. **`logSagaError` is deleted from both packages.** A saga failure is returned to `Resume`'s caller,
   which is the orchestrator, which decides between retrying and halting (ADR 0003).
6. **The settlement RPCs keep their own transition.** `PostPendingTransfer`, `CancelStagedTransfer`
   and `CancelAcceptedTransfer` still perform one claimed transition and answer with its outcome;
   `ConfirmStagedTransfer` remains a pure event-log write.

## Why the settlement RPCs are not an exception

The tempting formulation — "they are not saga dispatch" — is false, and stating it that way would
get the boundary tidied away by the next reader. `transaction.rollbackChild` calls
`CancelStagedTransfer` and `CancelAcceptedTransfer` from inside the saga; they *are* saga steps.

The line is narrower and holds: **they perform one claimed transition and return. They never fold.**
That is what separates them from `RequestTransfer` and `StartInitializingTransaction`, which
dispatched an open-ended amount of work; and it is why `rollbackChild` calling them is consistent
rather than a violation. Each is one bounded TigerBeetle post or void for one Transfer, called by
Ruby reacting to something the provider did, and its response is the answer the caller needs.

`StartProcessingTransfer` sits on the same line for the same reason: it requests exactly one gated
child, which nothing else can reach — a gated child is already touched, so `readyToRun` never
returns it. It no longer folds afterwards, so its response reports that the child was requested,
never that it finished.

## Consequences

**`INITIALIZED` becomes a state that lasts, and that is a gain.** It used to exist for microseconds.
It now means "accepted, waiting for the orchestrator", so a Transaction sitting in it is a visible
signal that publication or the orchestrator is behind — something that was previously invisible.

**Publication completeness becomes a liveness requirement, not only a consistency one.** Root ADR
0001 already promises it and ADR 0001 already warns that trigger semantics tolerates disorder and
duplication but not loss. Before this, a lost message cost lateness. Now it costs an aggregate that
never moves again, because nothing else will ever nudge it.

**That opens a recoverability gap, and `cmd/resume` closes it.** The connector runs
`snapshot.mode: no_data` and publication starts at the current end of the log, so anything written
before `make cdc-up` — or while the connector was down — is never published at all. Previously the
next RPC touching the id resumed it. `cmd/resume` drives one aggregate, or every aggregate still in
flight, through the real orchestrator, out of band the way `make migrate` is. It is also the first
concrete answer to ADR 0003's own open question about what an operator can do with a halted consumer.

**Every Transfer now costs a CDC round trip of its own.** A Transaction dispatching a child only
accepts it; running it takes the trigger that acceptance publishes. An ACH deposit that used to
reach its wait state in one call now takes several hops. This is latency, not work: the same folds
happen, spread across more messages.

**Deploy `ruby/` before `go/`, and run `cmd/resume -open` after.** Go-first breaks every in-flight
`Initiate`: the old code's second call gets `initialized` back, raises, and the user sees an error
while Go goes on to move the money — and then confirms a staged leg the new Go has not staged,
writing a durable `ConfirmStagedTransferRejected`. Ruby-first is safe, because the new submission
sweep run against old Go finds legs the inline saga already staged. Afterwards, work the old code
left mid-saga — a Transaction at `started` whose child is merely `accepted`, or one part-way through
a rollback — has no trigger coming, and `-open` is what gives it one.

**`cmd/simulate` now depends on the orchestrator rather than benefiting from it.** Its waiting
defaults were sized for a rescue and are now the normal path.

**The tests say what drives them.** Forty-four assumed an RPC ran the saga. Rather than hide that in
a wrapper, each names its driver — which is the property this decision establishes, so the diff is
the documentation. Two changed meaning rather than shape:
`TestOrchestrator_IsANoOpWhenTheSynchronousPathAlreadyFinished` was deleted, because there is no
synchronous path and redelivery already covers what it proved; and
`TestRequestTransfer_LogsSwallowedSagaErrors` became
`TestRequestTransfer_AcceptsDurablyAndSurfacesTheSagaFailureToItsDriver`, the guarantee having
inverted from "logged and swallowed" to "returned to the driver".

## This is not a throughput change, and the evidence points the other way

ADR 0004 records an informal investigation finding **Postgres's WAL fsync path, not synchronous
dispatch, to be the sustained-throughput ceiling** on this system's infrastructure. That finding
weakens the throughput case for this change and is not being set aside: the motivation is bounded
units of work, bounded blast radius, and a response that does not claim more than it knows. If
anything, the extra CDC round trip per Transfer costs latency that the old path did not.

## Not decided here

- Whether `cmd/resume` should ever be able to skip a message the orchestrator halted on. It drives
  aggregates; it does not touch consumer offsets.
- The O(N²) fold. `foldChildStates` decodes every event on the stream and `appendSagaStep` reloads
  and rescans it per append, so a Transaction's own cost is quadratic in its children — and async
  pays it once per published event rather than once per call. ADR 0007's bounds are what keep it
  tolerable; making the fold cheap is a separate change against a measurement nobody has taken yet.
