# 12. A ready child is always requested: manual gating is removed

- **Status:** Accepted
- **Date:** 2026-09-24
- **Scope:** `go/` context, and the published language it shares with `ruby/`. `TransferGatedWithinTransaction`,
  the `StartProcessingTransfer` RPC and its request, response and rejection messages are deleted.
  `Transfer.auto_process` (field 5) is reserved. Ruby stops setting the field and stops mapping the event
  (`event_state_map.rb`). Nothing Ruby projects changes, because it mapped the event to no state.
- **See also:** [ADR 0006](0006-synchronous-dispatch-removed-from-the-rpc-surface.md), which explained why
  `StartProcessingTransfer` could still dispatch. [ADR 0011](0011-a-transaction-records-dispatch-intent-before-the-request.md),
  which had to fence it against rollback like any other dispatch. [ADR 0004](0004-transaction-accept-time-funding-preflight.md),
  whose pre-flight skipped gated children.

## Context

A child with `auto_process=false` was not requested once its parents completed. The saga recorded
`TransferGatedWithinTransaction` instead, and the child waited for someone to call `StartProcessingTransfer`.

Nothing ever did:

- Every shape Ruby builds (ACH transaction and clearing, and every securities leg through `Securities::Leg`) sets
  `auto_process: true`, and so does `cmd/simulate`. Ruby never calls `StartProcessingTransfer`.
- The dev event log holds about 1.83 million Transaction events. None of them is a `TransferGatedWithinTransaction`.
- No ADR or context doc plans a use for it. Where Ruby calls a leg "gated", it means a DAG dependency, not this flag.

Even unused, the path had costs:

- **An RPC that dispatches.** ADR 0006 had to argue that `StartProcessingTransfer` requesting a child was not an
  exception to "no RPC drives a saga". ADR 0011 then had to give it its own intent-before-effect fence and a
  "no longer Started" refusal.
- **A workaround for publication lag.** A caller could arrive after the child was ready but before the saga had
  written `Gated`. `awaitingGate` answered that caller with a retryable Unavailable. It existed only because
  `Gated` recorded something the DAG already implied.
- **Distortions elsewhere.** Rollback recorded ABANDONED for gated children, `wouldAcceptReadyChildren` skipped
  them, and tests used gated children as a way to avoid needing a transfer client. That last habit left the real
  request path out of the slicing tests.

## Decision

**A child is requested as soon as its parents complete.** `dispatchReady` records a
`TransferRequestedWithinTransaction` intent for every child in the slice and requests each one (ADR 0011).
There is no other kind of dispatch.

Deleted:

- `TransferGatedWithinTransaction`, `childGated`, and its cases in `foldChildStates` and `childEventTransferID`.
- `StartProcessingTransfer`, `StartProcessingTransferRequest`/`Response`/`Rejected`, `awaitingGate` and
  `rejectedProcessing`.
- `Transfer.auto_process`, now `reserved 5; reserved "auto_process";`.

`wouldAcceptReadyChildren` now pre-flights every ready child except mint_source ones. For every caller that exists,
that is the same set as before, since all of them set `auto_process: true`.

The ABANDONED rollback method stays. It still records a child that moved no money: one rejected at accept, or one
that failed or was cancelled on its own.

## Consequences

**A Transaction has one fewer event type and no dispatching RPC.** Its public surface is now
`StartInitializingTransaction`, `GetTransactionState` and `StartTransactionRollback`. Only the orchestrator's fold
requests children.

**Wire compatibility holds both ways.**
- Older `TransactionInitialized` events carry `auto_process=true` in field 5. They still decode, and the field is
  ignored.
- A Ruby build that still sends the field is read the same way, because proto3 skips unknown fields.
- No stream holds a `TransferGatedWithinTransaction`, so deleting the message strands nothing.

**Tests exercise real dispatch.** The slicing and accept tests request children through
`acceptingTransferClient`. The rollback slicing tests seed children that were requested and failed. They no longer
use gated children.

**If a hold is ever needed, it should be designed again, not restored.** A child that must wait for an outside
signal is a business rule about that signal. It should be modeled where the signal lives, as a Transfer that stages
and waits to be confirmed, the way ACH settlement already works. It should not be a flag that makes the Transaction
wait for a second command.
