# 3. ACH clearing is a scheduled sweep that originates a derived-id follow-on

- **Status:** Accepted
- **Date:** 2026-09-18
- **Scope:** `ruby/` context only.
- **See also:** [ADR 0001](0001-read-model-consumer-and-follow-on-write-back.md) decision 4 and its warning on
  follow-on chains; [ADR 0002](0002-projection-consumer-topology-and-halt-policy.md).

## Context

An ACH deposit's shadow leg mints its amount into the entity's **uncleared cash**. Until the ACH return window
has passed, that money can still be clawed back, so it must not count as cleared. Nothing moved it on: a
withdrawal's shadow leg draws on **cleared cash**, so an entity funded only by deposits could never withdraw
(the "known gap" in `docs/ach-transactions.md`).

Clearing is Ruby's first follow-on Transaction: Go events reach Ruby, and Ruby originates new work in Go.
ADR 0001 requires deciding what terminates such a chain before shipping one.

## Decision

1. **A deposit clears from the start of the third Federal Reserve business day after its Transaction
   completed** (`Services::Ach::ClearingPolicy`). Business days skip weekends and Fed holidays as the Reserve
   Banks observe them — a Sunday holiday moves to Monday, a Saturday one does not move to Friday
   (`holidays` gem, `:federalreservebanks`). Dates are Eastern time.
2. **"Completed" is Go's time.** The CDC envelope carries `events.occurred_at`; the projection stores it as
   `state_changed_at`. Consumer lag or a replay does not move the clock. Rows projected before the envelope
   carried it fall back to projection time.
3. **Clearing is a Go Transaction** (`ach_clearing`, v1) with one Transfer, uncleared cash → cleared cash,
   for the deposit's amount. Not staged, not minting.
4. **Its ids are derived from the deposit** (`Services::DetId`, a byte-for-byte port of `go/internal/detid`):
   `detid(<ach id>:clearing)` and `detid(<ach id>:clearing:transfer)`. Go's idempotency on
   `StartInitializingTransaction` is the only duplicate guard, as ADR 0001 prescribes. Ruby records the ids,
   but only as a record.
5. **A recurring sweep, not a delayed job per deposit.** resque-scheduler runs `Jobs::ClearAchDeposits`
   hourly on weekdays (`config/resque_schedule.yml`); it clears every completed deposit that is due and
   whose clearing the read model has not yet seen. Nothing per-deposit lives only in Redis, so a lost Redis,
   a dead worker or a failed run is caught up by the next sweep. A clearing the read model has seen, in any
   state, is left alone — a rejected or rolled-back one needs a person, not a retry loop. One deposit failing
   does not stop the others; the run then fails, so it lands in Resque's failed queue.

## What terminates the chain

Only an **ACH deposit** triggers a clearing, and a clearing is not an ACH Transaction: nothing reacts to a
clearing Transaction completing. The chain is exactly one step long by construction. Any future follow-on
that reacts to clearings must revisit this.

## Consequences

- Withdrawals have cleared cash to draw on once deposits have cleared.
- Clearing lags by up to an hour past the start of its due day; that is the price of a sweep.
- The sweep reads the lagging read model: a consumer outage delays clearing (never duplicates it).
- Redis is operational state only (queues, schedule), with no persistence.
