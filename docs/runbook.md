# Runbook

## Service level objectives

| SLO | Target | Measured by |
|---|---|---|
| Availability: non-4xx responses | 99.5% over 30 days | `llm_gateway_requests_total` |
| Gateway overhead (excluding the model) | p99 < 20 ms | access log `ms` minus upstream `seconds` |
| Personal data sent to a hosted provider | 0 | tests in CI, `X-Gateway-PII` audit |

The latency of the models themselves isn't an SLO, because the gateway can't
control it. It is still alerted on (`GatewaySlowResponses`) because users
feel it.

## Useful commands

```bash
# Reload config without a restart (Kubernetes: update the ConfigMap, then)
kubectl exec deploy/llm-gateway -- kill -HUP 1

# Follow one request through the logs
kubectl logs -l app=llm-gateway | jq 'select(.request_id=="<id>")'

# Current breaker states
curl -s gateway:8080/metrics | grep breaker_open
```

## High error rate

1. Check `llm_gateway_requests_total{status="error"}` by provider. If one
   provider is responsible, its breaker should already be routing around it.
2. Check the provider's status page.
3. If every provider is failing, check egress: DNS, the NetworkPolicy and any
   proxy. `kubectl exec` into a pod and curl the provider.
4. If the errors are 401/403, an API key has expired or been rotated. Update
   the Secret and restart the pods (secrets are read at startup).

## Circuit breaker open

Expected during an upstream outage. Traffic goes to the next target. Act if:

- the provider is healthy but the breaker keeps tripping. This usually means
  the timeout is too short for the model. Raise `timeout_seconds` and reload.
- there is no fallback target for an alias. Add one, or tell the alias's users.

## Slow responses

1. Is it one provider? Compare p95 by provider on the dashboard.
2. For Ollama: check GPU memory. A model that doesn't fit in memory falls back
   to CPU and becomes 10–50x slower.
3. Check `llm_gateway_in_flight`. If it is near `max_in_flight`, the pods are
   saturated: scale out.

## Load shedding

The gateway is refusing requests because every in-flight slot is taken.

1. Check whether the HPA is scaling up, and whether it is already at
   `maxReplicas`.
2. If upstream latency has jumped, the same traffic needs more slots: check
   for slow responses first.
3. Raise `limits.max_in_flight` only if CPU and memory have headroom.

## Shared limits unavailable

`llm_gateway_shared_state_errors_total` is rising: replicas can't reach Redis
for rate limits or budgets, so each is enforcing its own count. Limits are up
to N times looser until Redis is back; nothing is refused because of it.

1. Check Redis (the cache's `redis_addr`): is it up, and is the gateway's
   readiness probe failing too?
2. If Redis will be down for long, lower `rpm` on the busiest keys for the
   duration.

## Spend spike

1. Dashboard: "Spend by key this month" shows which key it is.
2. Check that key's request rate, and whether it is hitting the cache.
3. Set or lower that key's `monthly_budget_usd` and reload. It takes effect
   on the next request.

## Config reload failed

The previous config is still running, so nothing is broken. The log line
`config reload failed` contains the validation errors. Fix the ConfigMap and
send `SIGHUP` again.
