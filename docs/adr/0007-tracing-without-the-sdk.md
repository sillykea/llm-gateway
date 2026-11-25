# 7. Trace with W3C trace context and OTLP, without the OpenTelemetry SDK

Status: accepted

## Context

Metrics say a provider is slow; a trace says which request, which attempt,
and where the time went. A request through the gateway can make several
provider calls (fallback), and the slow part is usually inside the model
server, so the trace has to continue into vLLM or Ollama.

The OpenTelemetry Go SDK would bring in a few dozen modules, against the
standard-library-only rule (ADR 1) that keeps the image small and the supply
chain short.

## Decision

`internal/tracing` implements the small part the gateway needs:

- W3C `traceparent` in (continue the caller's trace) and out (to every
  provider call), so the trace runs end to end with any OpenTelemetry-aware
  service on either side.
- One server span per API request and one client span per provider attempt,
  with OpenTelemetry's `gen_ai.*` attribute names (`gen_ai.request.model`,
  `gen_ai.usage.input_tokens`, ...) plus the gateway's own (`gateway.cache_hit`,
  `gateway.pii`, `gateway.rejected`).
- Export as OTLP/HTTP JSON to `<endpoint>/v1/traces`, which every
  OpenTelemetry collector, Jaeger, Tempo and Honeycomb accept.
- A bounded queue: when the collector is slow or down, spans are dropped and
  counted (`llm_gateway_spans_total{result="dropped"}`), never queued
  without limit and never on the request path.
- Head sampling by `tracing.sample_ratio`, decided on the trace ID; a
  caller's decision in `traceparent` always wins, so a trace is never
  half-recorded.

Prompts and completions are never put on spans: they may hold personal data.

## Consequences

- About 400 lines including tests, no new dependencies.
- No metrics or logs over OTLP, no span events or links, no tail sampling.
  If those are needed, the SDK becomes worth its weight, and swapping it in
  touches only `internal/tracing` and the few `Start`/`Set` calls.
