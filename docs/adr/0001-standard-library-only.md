# 1. Use only the Go standard library

Status: accepted

## Context

A gateway sits in the path of every model call and handles API keys for
several providers. Each dependency adds supply-chain risk, upgrade work and
image size. The obvious candidates were the Prometheus client, a Redis client
and an HTTP router.

## Decision

Use the standard library only. Go 1.22's `ServeMux` handles method and path
routing. The Redis client implements the few RESP commands needed (GET,
SET EX, PING, AUTH) over plain TCP or TLS, and metrics are written in the Prometheus text format by hand.

## Consequences

- The binary is small and the image is distroless with nothing else in it.
  `govulncheck` only has the toolchain to check.
- Around 250 lines of code are ours to maintain. They are covered by tests,
  including a fake Redis server.
- TLS and password auth come from `crypto/tls` and one extra command. Cluster
  mode and Sentinel aren't supported. If they were needed, switching to
  `go-redis` would only touch `internal/cache`, because the rest of the code
  depends on the `Cache` interface.
