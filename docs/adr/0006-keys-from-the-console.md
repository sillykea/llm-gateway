# 6. Pull API keys from the console, as hashes

Status: accepted

## Context

API keys live in the config file, each naming an environment variable that
holds the secret. Adding or revoking a key means editing the config, updating
a Kubernetes Secret and reloading every replica. That is fine for a handful
of service keys but not for teams creating and revoking their own keys.

## Decision

The gateway can pull extra keys from the console (`key_source`):

- The console stores and serves only the SHA-256 of each key. The gateway
  hashes the bearer token and looks the hash up. Keys are 256 bits of random
  data, so a hash can't be brute-forced back into a key.
- The gateway pulls; the console never pushes. The console needs no access
  to the gateway, and the gateway keeps working when the console is down.
- Polling every `refresh_seconds` (default 30) with an ETag. Most polls are a
  304 with an empty body.
- Fail-static: a failed or invalid response keeps the last good key set. The
  whole list is validated before it replaces the old one.
- A gateway that has never synced reports not ready, so a new pod doesn't
  take traffic and reject every console key.
- Config-file keys still work and are checked first. A console key can't
  reuse a config-file key's name, since limits and budgets are tracked by
  name.

## Consequences

- Revocation takes up to `refresh_seconds` to reach every replica. For an
  emergency, scale the poll down or roll the pods.
- If the console is down for a long time, new keys don't appear and revoked
  keys keep working. `llm_gateway_key_sync_last_success_seconds` makes that
  visible.
- Looking a key up by its hash in a map is not constant-time, but the timing
  depends on the hash, which an attacker can't steer towards a real key.
