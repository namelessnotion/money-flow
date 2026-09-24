# 9. Operations are a Transfer's legs, not aggregates of their own

- **Status:** Accepted
- **Date:** 2026-09-24
- **Scope:** `go/` context. The published language loses `proto/operation/v1` entirely and `TransferLeg`
  renames one field. Ruby consumed neither: it never called `OperationService` and never subscribed to
  `operation-events`.
- **See also:** [ADR 0005](0005-saga-dispatch-claim-prevents-double-invocation.md). Its contradiction tripwire
  used to be `operation/server.go`'s terminal-state guard, and that guard now lives on the Transfer.
  [ADR 0008](0008-transfer-legs-span-ledger-batches.md) is where legs already had to be reasoned about as the
  Transfer's own.

## Context

Throughput is bounded by how many Postgres commits a Transfer costs. Every commit waits on a WAL flush, and
sustained runs spend most backend time in `LWLock:WALWrite` / `IO:WalSync`. Measured on two machines, throughput
is roughly the WAL flush rate divided by the flushes per Transfer.

An immediate Transfer with one source Token made **8 commits and 12 events**:

| Commit | Events |
|---|---|
| accept | `TransferRequestAccepted` |
| `prepare()` AppendAtomic | `TokenMinted`, `TokenMintedForWallet`, `operation.Initiated` ×2, `TransferPrepared` |
| `commit()` claim | `TransferCommittingStarted` |
| `token.RecordBalances` ×2 | `TokenBalanceRecorded` ×2 |
| `operation.Perform` ×2 | `operation.Performed` ×2 (DEBIT, CREDIT) |
| `appendSagaStep` | `TransferCommitted` |

The staged path also wrote `operation.Staged` ×2, and every extra source Token added one more Operation per
step.

Every Operation fact was a restatement of a Transfer fact:

- `Initiated` repeats `TransferPrepared.legs`: same Tokens, same amount, same TigerBeetle id.
- `Staged`, `Performed`, `Cancelled` and `Failed` repeat `TransferStaged`, `TransferCommitted`,
  `TransferCancelled`/`PreparedTransferCancelled` and `TransferFailed`. Every leg of a Transfer moved in
  lockstep (`forEachOperation`), because TigerBeetle applies a chain of legs all-or-nothing and nothing else
  moved them.
- The CREDIT Operation never touched TigerBeetle. It was bookkeeping for bookkeeping.

Nothing read them. Stage, commit and reversal all read legs off `TransferPrepared`. No Go code loaded an
Operation stream outside the Operation package's own idempotency check. Ruby called neither RPC and consumed
no Operation events.

An Aggregate's boundary is where its invariants are enforced. An Operation was created in the same atomic
write as its Transfer and changed only by the Transfer's saga. Its one invariant, "never two different
terminal facts", is a Transfer invariant, because all legs share one outcome. So the Operation was an Entity
inside the Transfer that had been given a stream of its own, and it was paying for that stream in commits.

## Decision

**A leg (`TransferLeg`) is an Entity inside the Transfer aggregate.** The Transfer's stream is the whole
record of what its legs did. The Operation aggregate, its package, its `OperationService` RPC and
`proto/operation/v1` are deleted.

- `TransferLeg.debit_operation_id` is renamed **`ledger_transfer_id`**. That is what it always was: the leg's
  TigerBeetle transfer id, from which a staged leg's post and void ids are derived. The field number (4) is
  unchanged, so older `TransferPrepared` events still decode.
- `TransferLeg.credit_operation_id` and `TransferDestination.credit_operation_id` are removed and their numbers
  reserved. `TransferRequestAccepted.debit_operation_ids` and `ReversalRequestAccepted.debit_operation_ids`
  were never populated, and are removed and reserved too.
- `TransferFailed` and `PreparedTransferCancelled` gain a `reason`. That fact used to live only on
  `operation.Failed` and `operation.Cancelled`.

**The Transfer enforces its own lifecycle.** `transitions` in `saga.go` lists, for each state, the outcomes
that may follow:

- Accepted → AcceptedCancelled
- Prepared → Staged | Committed | Failed | PreparedCancelled
- Staged → Pending | Cancelled
- Pending → Committed | Failed | Cancelled

`appendSagaStep` converges when the wanted outcome is already the Transfer's latest outcome, looking past claim
markers and `*Rejected` responses rather than only at the tail. It refuses anything else with
`already <state>, cannot also become <event>`.

This replaces `operation/server.go`'s guard as the tripwire ADR 0005's halts relied on, and it is stricter.
The old guard caught only a leg reaching two different terminal facts. This one also refuses Committed → Failed,
Staged → Committed without Pending, and an Accepted-state cancel landing after `TransferPrepared`. That last
case had been a live gap: `cancelPrepared`'s Accepted branch used to append without re-checking state. It now
appends only onto the stream exactly as it loaded it, and a lost race sends `CancelAcceptedTransfer` back to
re-decide.

**Unchanged:** the claim markers, `claimForDispatch`, `requireClaim`, the rule that only the same transition may
take over a stale claim, and `CancellingPreparedTransferStarted`. The claims still guard the TigerBeetle side
effects they were built for. What changed is that a claimed step has less to do after TigerBeetle answers: it
records balances (idempotent) and appends one guarded outcome, where it used to walk every leg's Operation.

## Consequences

**The immediate path costs 6 commits and 8 events instead of 8 and 12.** The staged path saves 4 commits.
Every extra leg saves one more commit per step, plus an `Initiated` event in `prepare()`. `requireClaim` also
drops one `Load` per step, and each leg drops the `Load` its Operation step made.

Measured on 2026-09-24 on the local Docker Desktop stack. Each run was `cmd/simulate` with `-entities 150
-transactions 3000 -concurrency 48`, with the ledger check on, and each run was done twice. simulate's
Transfers are all staged, which is the path that wrote the most Operation events (six per Transfer):

| | before | after |
|---|---|---|
| transfer mode | 286.2, 284.6 /s | 317.6, 326.0 /s (**+12%**) |
| transaction mode | 223.5, 222.7 /s | 239.4, 235.6 /s (**+6%**) |
| events written, transfer-mode run | ~58k (18,600 of them Operation) | ~40k |
| WAL records, transfer-mode run | 300k, 326k | 214k, 230k |
| WAL flushes (`wal_sync`), transfer-mode run | 31.7k, 34.7k | 27.1k, 29.2k |

The gain is smaller than the commit count suggests. Concurrent commits were already sharing flushes (group
commit), so removing commits cut flushes by only ~15%. The runs are short and on one machine; treat the numbers
as direction, not capacity.

`TestImmediateTransfer_RecordsItsLegsOnItsOwnStreamOnly` pins the commit budget and the aggregate types a
Transfer writes to. A change that quietly adds a commit back fails it.

**The middle of a claimed step shrinks.** ADR 0005 Round 4 worried about a slow `cancelPrepared()` still
cancelling later legs while another transition took over. That window no longer exists, because a prepared
cancel is now one append. The stale-takeover rule is kept, because TigerBeetle round trips, and a wide
Transfer's run of chains, still take real time.

**Historical `operation.v1.*` rows no longer decode.** The event log is append-only, so existing dev databases
keep their Operation streams. `cmd/events` shows those rows as `(undecodable: …)`, and nothing else loads them.
The `operation-events` Kafka topic stops receiving messages.

**Follow-up, not done here:** with no per-leg writes left, `cancelPrepared()`'s `CancellingPreparedTransferStarted`
marker guards nothing but its own single append. The marker could be replaced by waiting out any live claim and
appending the outcome by compare-and-swap. That saves one commit on the cancel path only, and it means
revisiting `claimedPreparedCancel`.
