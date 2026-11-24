# Threat model

What the gateway protects, who it protects it from, and what it doesn't do.

| Threat | Mitigation | Gaps |
|---|---|---|
| Unauthorised use of model credit | Bearer key per client, constant-time compare | Keys are static; there's no rotation endpoint |
| A runaway client draining the budget | Per-key rate limit and monthly budget | Per replica (ADR 3) |
| Personal data reaching third parties | Detection before routing, fail closed (ADR 2) | Patterns only: names and addresses aren't detected |
| Provider keys leaking | Read from env (Kubernetes Secrets), never logged or returned | Anyone with pod exec can read env |
| Prompt contents in logs | Logs record metadata only, never message text | Upstream providers may log prompts themselves |
| Request smuggling or oversized bodies | 1 MiB body limit, `ReadHeaderTimeout` | No per-key payload limit |
| One bad request crashing the service | Panic recovery per request | — |
| Cache poisoning across privacy modes | Cache key includes the routing decision | Cache entries aren't scoped to a key; responses are shared between keys for identical prompts |
| Lateral movement from a compromised pod | Non-root, read-only filesystem, distroless, NetworkPolicy | — |

The last cache gap is deliberate: identical deterministic prompts get
identical answers, which is where the savings come from. For a multi-tenant
deployment where that isn't acceptable, add the key name to the cache key.
