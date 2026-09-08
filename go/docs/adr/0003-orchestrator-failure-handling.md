# 3. A trigger the orchestrator cannot process halts the consumer

- **Status:** Accepted
- **Date:** 2026-09-08
- **Scope:** `go/` context only.
- **See also:** [ADR 0001](0001-event-triggered-saga-orchestrator.md), which settles what a message *means* but
  not what happens when handling one keeps failing.
  [Root ADR 0001](../../../docs/adr/0001-async-event-driven-saga-and-postgres-to-kafka-publication.md) decision 6,
  which settles the consumer topology this decision operates within.

## Context

ADR 0001 establishes that a message is a trigger, that idempotency lives on the write side, and that
at-least-once delivery is sufficient. It is silent on the failure case, and the failure case is where a
money-moving consumer earns or loses its reputation: **what happens to a message whose handler keeps failing?**

Three answers are conventional, and the differences between them are not stylistic.

- **Retry forever.** Nothing is lost. The partition stops advancing anyway, so the availability cost is the
  same as halting — but nothing announces it, and the failure looks like a system that is merely quiet.
- **Dead-letter.** Availability is preserved and the message is kept for later. But under trigger semantics a
  message is not a payload to reprocess: it is the *only wake-up* that aggregate was going to get. Moving it
  aside leaves a Transaction parked indefinitely while every dashboard reports the consumer healthy. The
  aggregate does not fail; it goes quiet, which is worse.
- **Halt.** The consumer stops on the message, having committed nothing past it. Availability is lost loudly.

The asymmetry that decides it: a trigger's *retry* is free — re-folding authoritative state converges — but a
trigger's *loss* is invisible. So the cost of trying again is near zero, and the cost of moving on is a stall
nobody is looking for.

## Decision

1. **A message that cannot be processed halts the consumer.** It is never committed, never skipped, never
   dead-lettered. The next start resumes on it.
2. **Bounded retries first.** A handler failure is retried a small number of times (three, doubling from
   250ms) before the halt. This rides out a database blip without turning every blip into an incident.
3. **A message that cannot be parsed is not retried at all**, but still halts. Retrying exists to outlast a
   transient fault in something the handler depends on; a malformed envelope is not transient, and no number of
   attempts changes it.
4. **Offsets are committed after the side effects, one message at a time.** This is what makes delivery
   at-least-once, which ADR 0001 decision 3 permits and this decision depends on: a crash between the side
   effect and the commit redelivers, and redelivery re-folds.
5. **A failure to commit also stops the consumer.** A consumer that cannot record progress will replay from its
   last commit on every restart; consuming through that is not resilience, it is an unbounded replay nobody
   ordered.

## Why the orchestrator is a separate binary

This decision is only tolerable because halting the orchestrator does not take the API down with it.
`cmd/orchestrator` is therefore its own process rather than a goroutine inside `cmd/server`: the RPC server
keeps answering while saga progress is stopped, which turns "the orchestrator has halted" from an outage into a
degradation. The two also have genuinely different scaling and restart profiles, and staging the eventual
cutover needs the consumer deployed and observed before the synchronous dispatch is removed.

## Consequences

**Halting is coarser than one partition.** The consumer stops reading every partition it owns, not only the one
carrying the bad message. Pausing a single partition is possible and is the obvious refinement, but it buys
availability for the aggregates that share a topic with a broken one — and nothing is in production, no cutover
has happened, and a persistently failing handler in a money system is an incident rather than a background
condition. Revisit when a halt has actually cost something.

**One topic halting cancels the other.** `cmd/orchestrator` stops both consumers when either gives up. A
Transaction that cannot hear from the transfer topic is not usefully still working, and one visible failure is
easier to act on than a half-running system that looks healthy.

**Alerting has to distinguish stopped from quiet.** The orchestrator exits non-zero and logs the exact
topic/partition/offset it halted on, so the signal exists — but an event log is low-volume and legitimately
idle for long stretches, so "no progress" alone means nothing. Alert on the process exiting and on consumer-group
lag, not on silence. ADR 0002 makes the same point about `rollback_started`, for the same reason.

**Redelivery is normal, not exceptional.** Every restart after a halt reprocesses at least one message, and a
retry reprocesses the same trigger several times. This is safe only because ADR 0001's write-side idempotency
holds, so anything that weakens `appendSagaStep`'s guard weakens this decision too.

**Not decided here:** what an operator should *do* with a halted consumer beyond looking at it — whether there
is ever a supported way to skip a message by hand, and what that would have to record. Nothing in the code
offers one today, deliberately.
