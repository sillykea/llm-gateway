# 3. Keep rate limits, budgets and breakers per replica (for now)

Status: accepted; amended by the update below, which adds shared limits as an option

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

## Update: shared limits are implemented

`limits.shared: true` (with `cache.backend: "redis"`) moves rate limits and
budgets to the cache's Redis, as designed above:

- Rate limits run the same token bucket as a Lua script (`internal/ratelimit/shared.go`).
  It reads Redis's clock, not the replica's, and runs atomically, so two
  replicas can't both spend the last token. One `EVALSHA` per request.
- Budgets are an `INCRBYFLOAT` per key per month, expiring after 40 days.
  Checking a budget is a `GET`; keys without a budget skip it.
- Both have a 200 ms timeout. If Redis fails, that replica falls back to its
  own in-memory count, so limits loosen to the per-replica behaviour rather
  than every request failing. Each failure increments
  `llm_gateway_shared_state_errors_total`; spend recorded during an outage is
  kept locally and still counts against the budget.
- Breakers stay per replica, for the reason above.

Tested with three limiter instances on one Redis: a burst of 90 requests
against a 30 rpm key lets 30 through, not 90.
