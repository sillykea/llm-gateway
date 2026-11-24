# 2. Fail closed on personal data, fail open on the cache

Status: accepted

## Context

The gateway has two optional dependencies whose failure changes behaviour:
the local model pool (needed for personal data) and the cache.

## Decision

- If a request contains personal data in `route_local` mode and no local
  provider is configured or available, return 422. Never fall back to a
  hosted provider.
- If the cache is unavailable, treat every lookup as a miss and carry on.

## Consequences

Privacy guarantees hold even during an outage, at the cost of failing some
requests. That trade is right because a leaked card number can't be undone,
but a failed request can be retried. The cache is a pure optimisation, so
losing it only costs money and latency.

Detection is pattern-based, so it misses things like names and street
addresses. The guarantee is "known patterns never leave", not "nothing
personal ever leaves". The README says so plainly rather than overselling it.
