# 4. Only fall back before the first streamed byte

Status: accepted

## Context

With non-streamed requests, falling back to another provider is invisible
to the client. With streaming, the client may already have shown part of an
answer.

## Decision

Fall back only if a provider fails before the first chunk has been written.
After that, write an error event into the stream and stop.

## Consequences

Clients never get two half-answers from different models joined together,
which would be worse than an error. Clients must handle an error event in a
stream, which the OpenAI SDKs already do. Partial streams are still counted
for cost and budgets, because the tokens were generated and paid for.

Provider timeouts use `ResponseHeaderTimeout`, which covers connecting and
waiting for the first byte, not the whole response. The server has no
`WriteTimeout`. Either one would cut off long answers that are healthy.
