# 15. The command handlers are checked against a TLA+ design, and traces of the code against the design

- **Status:** Accepted
- **Date:** 2026-09-25
- **Scope:** `go/` context. No event, RPC or published language changes. Adds `internal/tlatrace` (used only by a
  test), a trace harness test in `internal/transfer`, and the specs under `spec/`.
- **See also:** `spec/eventlog.tla`, `spec/eventstore.tla`, `spec/Traceeventstore.tla`, and `make tla-check` /
  `make tla-trace`.

## Context

Every command handler that decides a new stream's first event follows one loop: Load the stream, answer with the
recorded outcome if the command was already decided, otherwise decide, and append at the loaded length.
`UNIQUE(aggregate_type, aggregate_id, sequence)` turns a stale length into `ErrConcurrencyConflict`, and the handler
reloads and re-decides, up to `maxConcurrencyAttempts`. The loop's correctness under concurrency, faults and lost
replies had only been argued in comments and exercised by a few race tests.

A spec that restates the Go loop and then checks properties written in the loop's own terms proves little: a flaw in
the loop and in its property cancel out.

## Decision

The loop is checked at two levels, each against something that shares no definitions with it.

1. **The design is checked against a contract.** `spec/eventlog.tla` states what callers and log readers are
   promised, with no mention of Workers, retries or sequences:
   - a command is decided at most once per id;
   - the log is append-only;
   - an answer is either the recorded decision or `Unknown`;
   - every request is answered.

   `spec/eventstore.tla` models the loop in an environment that behaves as badly as the real one can: requests in
   any order, failures anywhere, a COMMIT landing while its reply is lost. TLC checks that it refines the contract.
2. **The code is checked against the design.** `TestTLATrace` drives concurrent, same-id `RequestTransfer` and
   `RequestReversal` calls through real Twirp and the real Postgres store, and injects faults. `internal/tlatrace`
   records each step as the database answered it. `spec/Traceeventstore.tla` checks each trace is a behavior of
   `eventstore.tla`, using only eventstore's own actions conjoined with what the trace observed.

Both run in CI. TLC is vendored at `spec/tools/tla2tools.jar`, because the v1.8.0 release the specs need is rebuilt in
place and can't be pinned by URL.

## Consequences

- Breaking the loop is caught. Removing the reload after a lost race (answering the loser's own decision instead)
  gets rejected in every recorded trace where a race was lost with differing decisions.
- The design surfaced a limit of the loop as these two handlers use it. They only ever write a stream's first event,
  so a request loses at most one race, and the reload after it always finds the id decided. `maxConcurrencyAttempts`
  beyond 2 is unreachable for them, and so is `twirp.Aborted`, which the traces confirm.
- `RequestTransfer` and `RequestReversal` treat any non-empty stream as decided. That only answers correctly because
  of the type switch's `default` case in `decidedRequestTransfer` and `decidedRequestReversal`, which turns another
  command's decision into `twirp.Internal`. The model check shows the shortcut without that case answers a caller with
  another command's decision. A reused id makes every retry `Internal`.
- Traces are validated per stream, and each Request's own lines are the only order trusted. Lines are written after
  Postgres answers, so their order across Requests isn't commit order, and TLC searches for an interleaving that fits.
- Not covered yet:
  - the orchestrator's writes to Transfer streams, and `AppendAtomic` in general, which the design doesn't model;
  - holder `Establish` (a different loop, whose behaviors are a subset of the design's);
  - production wiring: `tlatrace` is installed only by the harness.
