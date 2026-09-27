# 17. OpenTelemetry at the ports

- **Status:** Accepted
- **Date:** 2026-09-27
- **Scope:** `go/` context: `cmd/server`, `cmd/orchestrator` and `cmd/resume`, a new `internal/telemetry`
  package, structured logging in `internal/saga` and `internal/saga/kafkareader`, and a local `lgtm` service in
  `docker-compose.yml`. No domain event, proto or aggregate changes.
- **See also:** [ADR 0003](0003-orchestrator-failure-handling.md), root [ADR 0001](../../../docs/adr/0001-async-event-driven-saga-and-postgres-to-kafka-publication.md),
  `docs/saga-orchestrator.md` ("Watching it").

## Context

The Go binaries had no instrumentation. They logged with the stdlib `log` package: one plain-text line per
handled trigger, and a line on a halt. ADR 0003 says to alert on consumer-group lag and not on silence, but
nothing measured lag. The runbook's way to find a halt was `docker logs | grep halt`. Nobody could see RPC
latency, how often appends lost an optimistic-concurrency race, what TigerBeetle rejected, or whether the Postgres
pool was saturated, short of attaching a debugger.

## Decision

1. **OpenTelemetry, exported over OTLP/HTTP.** Traces, metrics and logs share one SDK and one wire protocol, and
   the Ruby backend can join the same traces later without anything changing on the Go side. Metrics are pushed,
   not scraped. The orchestrator has no HTTP listener, and adding one only to be scraped would give it a second
   reason to fail.

2. **Export is on only when an OTLP endpoint is configured.** `telemetry.ConfigFromEnv` reads the standard
   `OTEL_EXPORTER_OTLP_*` variables and `OTEL_SDK_DISABLED`. With no endpoint, the providers are no-ops, so
   tests, CI and a bare `go run` need no configuration and emit no export errors. `LOG_LEVEL` sets the log level,
   and a value it doesn't recognize stops startup.

3. **Instrument the ports, not the domain.** `internal/telemetry` wraps each port in a decorator: the
   `eventstore.Store`, the `ledger.Client`, every Twirp server (through an interceptor), the HTTP mux and the
   `saga.Handler`. `internal/tlatrace` already uses this shape. `transfer`, `transaction`, `holder`, `wallet` and
   `token` do not import OpenTelemetry. The one exception is `contention.Wait`. It adds a span event to whatever
   span its context already carries, using only the trace API, so a hot stream's re-plan laps appear in the trace
   of the step that paid for them. The store and ledger decorators wrap explicitly rather than by embedding, so a
   method added to either interface fails to compile until someone decides how to measure it.

4. **No global providers.** Every decorator takes a `telemetry.Providers` value, and otelhttp and otelpgx are
   handed the same providers explicitly. Nothing calls `otel.SetTracerProvider`. A parallel test can observe one
   decorator's spans without interference from any other test, and nothing instruments itself from an import
   side effect.

5. **An answer is not a fault.** Only a failure of the system sets span status `Error`. When the system answers
   correctly, the answer is recorded as an outcome attribute:
   - A lost append race is `money_flow.outcome=conflict`.
   - A Twirp 4xx code, such as `aborted`, `not_found` or `failed_precondition`, is recorded in
     `twirp.error_code`.
   - A TigerBeetle result other than `ok` is counted by its code, such as `exceeds_credits`.

   A trace that turns red on every lost race would hide the real faults. Failures of the system are a 5xx Twirp
   code, a driver or transport error, a batch the ledger refused as a whole (`invalid_request`), and a saga
   attempt that returned an error.

6. **Aggregate identity links what trace context cannot.** Trace context doesn't survive the CDC hop, because
   the event log is immutable and Debezium publishes the row, not the request. So a saga step's span starts a
   new trace. Every RPC span, store span and saga span carries `money_flow.aggregate_id`, and saga spans also
   carry `money_flow.global_seq`, so the RPC that wrote an event and the saga steps it triggered can be found
   from each other. Carrying `traceparent` through the log would need an event-metadata column. That is a
   decision about the published language, and it is not made here.

7. **Structured JSON logs, correlated with traces.** The long-running binaries log with `log/slog` as JSON on
   stdout. Each line carries `service`, and a line written inside a span also carries `trace_id` and `span_id`.
   When exporting, the same records go over OTLP too. `saga.Consumer` and `kafkareader` take their
   `*slog.Logger` by injection, as they took a `*log.Logger` before. `Message` and `Trigger` log as structured
   groups. A halt still says `HALTED`, so the runbook's grep still finds it. `cmd/migrate`, `cmd/events`,
   `cmd/simulate` and `cmd/resume`'s own output stay plain text, because a person reads them at a terminal.

## What is measured

| Signal | Name | Dimensions |
|---|---|---|
| RPC latency | `rpc.server.call.duration` (s) | `rpc.service`, `rpc.method`, `twirp.error_code` |
| HTTP | otelhttp's `http.server.*` | method, status |
| Event store | `money_flow.eventstore.operation.duration` (s) | `money_flow.operation`, `money_flow.aggregate_type`, `money_flow.outcome` (`ok`, `conflict`, `duplicate_stream`, `error`) |
| Appends | `money_flow.eventstore.events.appended` | `money_flow.aggregate_type`, `money_flow.event_type` |
| Ledger | `money_flow.ledger.operation.duration` (s) | `money_flow.operation`, `money_flow.outcome` |
| Ledger rejections | `money_flow.ledger.rejections` | `money_flow.operation`, `money_flow.ledger.result` |
| Saga steps | `money_flow.saga.handle.duration` (s) | `money_flow.aggregate_type`, `money_flow.event_type`, `money_flow.outcome` |
| Consumer lag | `money_flow.saga.consumer.lag` (gauge) | `messaging.consumer.group.name`, `messaging.destination.name`, `messaging.destination.partition.id` |
| Postgres | otelpgx `pgxpool.*` and per-statement spans | |
| Runtime | `go.*` runtime metrics | |

Aggregate ids appear only on spans, never on metrics, where every new aggregate would start a new time series.

Consumer lag is measured from the brokers (`kafkareader.GroupLag`), not from the reader's own statistics, because
a consumer that has stopped fetching is exactly the case the reader can't report. A lag that can't be measured is
logged and reported as missing, never as zero, which would read as "caught up".

## Consequences

- Locally, `docker compose up` starts `lgtm` (`grafana/otel-lgtm`), and Grafana on `localhost:3001` shows traces,
  metrics and logs for `money-flow-server` and `money-flow-orchestrator`.
- To alert per ADR 0003, alert on `money_flow.saga.consumer.lag` growing, and on the orchestrator process being
  absent, which shows as its metrics going stale. An idle topic doesn't show as zero throughput; it shows as
  zero lag.
- A saga step that fails and is retried leaves one failed span per attempt before any halt. The halt log line
  carries the `trace_id` of the last attempt.
- `ledger.AccountResultCode` and `ledger.TransferResultCode` now name themselves (`exceeds_credits`, not `4`).
  The free-text reasons already built from them, in `TokenMintRejected` and a compensated Transfer's
  `TransferFailed`, now read that way too.
- The OpenTelemetry SDK, its OTLP exporters, otelhttp, otelpgx and the runtime instrumentation are new
  dependencies of the Go module.
