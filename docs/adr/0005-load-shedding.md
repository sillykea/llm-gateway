# 5. Shed load instead of queueing

Status: accepted

## Context

Model calls take seconds. When traffic goes over capacity, an unbounded
server queues goroutines and connections until every request is slow, clients
time out and retry, and the overload gets worse.

## Decision

Cap concurrent chat requests with a semaphore (`limits.max_in_flight`). When
it is full, answer 503 with `Retry-After: 1` straight away.

## Consequences

Under overload, some requests fail quickly instead of every request failing
slowly. Requests that do get in keep normal latency. The `load_shed_total`
metric and the `GatewayLoadShedding` alert make it visible, and the HPA adds
replicas. The cap is per replica, sized from the expected concurrency per pod
(rate × average latency) with headroom.
