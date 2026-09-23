# 4. StartInitializingTransaction pre-flight-checks its own ready children

- **Status:** Accepted
- **Date:** 2026-09-19
- **Scope:** `go/` context only. `ruby/docs/adr/0004` is the money-safety decision this exists to keep true; that
  ADR's own Scope and Consequences are amended alongside this one.
- **See also:** [ruby/docs/adr/0004](../../../ruby/docs/adr/0004-ach-withdrawal-funds-before-it-leaves.md), the
  decision this makes possible without depending on the synchronous saga described in
  [go/docs/adr/0001](0001-event-triggered-saga-orchestrator.md).

## Context

`ruby/docs/adr/0004` depends on `StartInitializingTransaction` running its saga **synchronously**: `Initiate`
asks Go to run a withdrawal's Transaction, then calls `ResumeTransaction` once more, and treats that second
call as definitive proof the withdrawal is still funded before ever submitting the entry to the bank. That
dependency on synchronous saga execution is exactly the kind of coupling a future move toward
[async orchestration](0001-event-triggered-saga-orchestrator.md) would put at risk — nothing about it requires
the saga to have run synchronously at all, it only requires *some* correct answer to exist before Ruby submits
anything.

`StartInitializingTransactionResponse` is built and returned from the accept/reject decision (`TransactionInitialized`
vs `TransactionRejected`) before `runSaga` ever runs — this was already true before this decision and remains
true after it. That is why `Initiate`'s `ResumeTransaction` call still has to exist: nothing this decision does
changes what that response reflects.

## Decision

1. **`StartInitializingTransaction` pre-flight-checks every immediately-dispatchable, non-mint_source child**
   before writing anything, the same way it already rejects a malformed DAG before writing anything. "Immediately
   dispatchable" means `auto_process=true` and ready at time zero per `readyToRun` (no unmet dependency) — for an
   ACH withdrawal (`ruby`'s `TransactionShape` v2), that is exactly the shadow leg, the one ADR-0004-on-the-ruby-side
   calls funding. If any such child would be rejected right now, the whole Transaction is rejected outright
   (`TransactionRejected`), and nothing else is ever written for it — no `TransactionInitialized`,
   no dispatch, no rollback cascade.
2. **The check reuses `transfer.selectSourceTokens`'s own decision, exposed read-only as
   `transfer.WouldAcceptTransfer`.** It performs the identical read (one batched TigerBeetle balance lookup) the
   real dispatch performs moments later, and writes nothing. It is not a reservation.
3. **`mint_source` children are excluded**, for two independent reasons, either of which would be sufficient on
   its own: they have no balance constraint at all (a mint_source leg mints a fresh, zero-balance Token and is
   expected to go negative — see `transfer.validateMintSource`), and `validateMintSource` calls
   `TransactionExistsChecker`, which would deterministically return false for a Transaction that has not been
   written yet — checking a mint_source child here would spuriously reject every one of them.
4. **This pre-check is additive, never authoritative.** The real dispatch inside `runSaga` is completely
   unchanged and still runs immediately afterward when the pre-check passes; it remains the sole source of
   truth. A concurrent, unrelated debit against the same wallet between this check and the real dispatch can
   still change the answer — this is not a reservation or a lock, and closing that race is out of scope (it
   would require one). `Initiate#require_running!`'s `ResumeTransaction` call therefore stays exactly as it was:
   it is the only thing that ever learns the real dispatch's actual outcome, since this pre-check's own decision
   never reaches `StartInitializingTransactionResponse` either way.

## Consequences

**The common underfunded-withdrawal case is now cheaper and cleaner, not just eventually consistent.** Before
this decision, an underfunded withdrawal wrote `TransactionInitialized` → `TransactionStarted` → a failed child
dispatch → `TransactionRollbackStarted` → `TransactionRolledBack`, and Ruby only learned about it from a second,
separate `ResumeTransaction` call. Now it writes one `TransactionRejected` event and nothing else, and Ruby's
very first call (`start_transaction`) already carries the answer — `require_running!` almost never has anything
new to report in this path anymore.

**Known limitation, not fixed here:** if a Transaction has *multiple* ready-at-zero, non-mint_source children
drawing from the *same* wallet simultaneously — not a shape either ACH direction produces today, but possible
for the mechanism in general — this check evaluates each independently against the same unconsumed balance
snapshot, which can over-accept relative to what the real, sequential dispatch would actually do. The real
dispatch remains authoritative and will still correctly reject/roll back in that residual case exactly as it
did before this decision; this pre-check is a "usually right, always safe" fast path, not a weakening of any
existing guarantee.

**One more TigerBeetle round trip on the ACH-withdrawal hot path.** The pre-check and the real dispatch both
call the identical balance read against the same wallet — a deliberate, accepted trade for the correctness/
architecture win above, not something this decision attempts to avoid.

**Not decided here:** removing the synchronous `runSaga` call from `StartInitializingTransaction` (or any other
RPC handler) entirely — the "full async cutover" root ADR 0001 anticipated. A throughput investigation
(informal, not itself an ADR) found Postgres's own WAL fsync path, not synchronous dispatch, to be the
sustained-throughput ceiling on this system's current infrastructure, which weakens the case for that further
change. This decision stands on its own regardless of whether that larger cutover ever happens.

> **Resolved 2026-09-22 by [ADR 0006](0006-synchronous-dispatch-removed-from-the-rpc-surface.md).** The cutover
> was made, and that ADR takes the throughput finding above at face value rather than setting it aside: the case
> it argues is bounded units of work and honest response semantics, not speed.
>
> **Decision 4's last sentence is superseded.** It says `Initiate#require_running!`'s `ResumeTransaction` call
> "therefore stays exactly as it was". It does not: under the cutover that call would only ever see
> `INITIALIZED`, because the real dispatch has not happened when the RPC returns. The check it performs is
> unchanged in substance and now lives in `Services::Ach::SubmitDue`, alongside a second condition that makes it
> stronger — see [`ruby/docs/adr/0008`](../../../ruby/docs/adr/0008-ach-submission-is-a-sweep.md).
>
> **Everything else here still holds**, including the known limitation below — which ADR 0006 makes more
> reachable, not less, since the gap between this pre-check and the real dispatch is now a CDC round trip rather
> than microseconds. [ADR 0007](0007-bounded-transaction-width-and-sliced-dispatch.md) bounds the damage.
