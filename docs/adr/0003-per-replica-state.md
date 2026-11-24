# 3. Keep rate limits, budgets and breakers per replica (for now)

Status: accepted

## Context

Rate limits, monthly budgets and circuit breakers all need state. With N
replicas, in-memory state means each replica enforces the limits on its own.

## Decision

Keep this state in memory, per replica, for the first version.

## Consequences

- An effective rate limit of up to N × `rpm`. Configure `rpm` per replica, or
  divide by the replica count.
- Budgets reset when a pod restarts and are enforced per replica, so they are
  a safety net against runaway scripts, not exact billing. The precise figure
  is in the `llm_gateway_cost_usd_total` metric, which Prometheus sums across
  replicas.
- Per-replica breakers are a *feature*: each replica learns about upstream
  health from its own traffic, and there is no shared state that could fail.

## Moving to shared state

When exact limits matter, move the rate limiter and budgets to Redis: an
`INCRBYFLOAT` per key per month for budgets, and a Lua token-bucket script
for rate limits, so each check stays one round trip. Both packages already
have narrow interfaces (`Allow`, `Add`/`Exceeded`), so the change doesn't
reach the handlers.
