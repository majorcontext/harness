# Provider instructions

These rules apply to `provider/` and its adapters. Harness does not merge
ancestor files. If root guidance is not active, locate the Git root and read
`<repo-root>/AGENTS.md`. Resolve repository paths from that root. Read
`message/AGENTS.md` for canonical data rules.

## Adapter boundary

Each adapter receives a canonical `provider.Request` and builds a new wire
request. Never store provider wire state in session history.

Every transcoder must:

- Call `message.NormalizeForWire`.
- Read tool output through `ToolResult.SafeContent`.
- Apply `imageclamp.Clamp` with adapter-specific limits.
- Map internal tool-call IDs deterministically.
- Replay opaque `ProviderData` only for the matching family.
- Preserve request order after same-role merging.
- Keep prompt-cache markers out of canonical history.

Use golden JSON tests for wire shape and ordering.

## Error classification

Classify errors with typed `provider.Error` values. Engine retry code must not
match provider error text.

Mark malformed requests permanent. Keep context overflow separate. Use text
matching only inside a provider parser for a documented provider shape, such as
context overflow or account exhaustion.

A stream that ends without its terminal event is
`RetryableStreamTruncated`. Do not report it as an ordinary cancellation.

## Images in tool results

Anthropic can recurse into tool-result blobs. Native OpenAI and
OpenAI-compatible adapters replace those blobs with an omission note. Preserve
this adapter difference until the wire contracts change.

Do not add a static model-name vision list. The repository has no complete
capability signal.

## Reasoning effort

`message.EffortUnset` means "send no control." It is not equal to
`message.EffortOff`.

- Anthropic maps enabled levels to thinking budgets. It raises `max_tokens`
  above the budget and drops temperature and top-p.
- Native OpenAI Responses maps enabled levels into `reasoning.effort`.
- OpenAI-compatible chat sends the literal `"off"` for `EffortOff`.
  Gateways can reason by default when the field is absent.

Reasoning-history stripping is intentionally asymmetric:

- Anthropic strips stored thinking when reasoning is not enabled.
- Native OpenAI strips stored reasoning only for explicit `EffortOff`.
- Native OpenAI must replay encrypted reasoning on `EffortUnset` for
  stateless multi-turn tool use.

Do not replace this with one shared `!Reasoning()` condition.

Read `docs/models-and-providers.md` before changing effort or
compaction request behavior.

## Session affinity

`Request.SessionKey` is the stable session routing hint.

- OpenAI-compatible sends `user` and, unless disabled, `prompt_cache_key`.
- Native OpenAI Responses sends `prompt_cache_key`.
- Anthropic ignores `SessionKey` and uses explicit cache markers.

Omit empty keys. Do not replace the gateway `user` field with the native
OpenAI field.

## Anthropic cache TTL

Anthropic uses two cache breakpoints. The default TTL is one hour.

- `"1h"` adds the TTL and the required beta header.
- `"5m"` restores the short cache shape without the beta header.
- Reject unknown values.
- Reject `cache_ttl` on an entry that does not build the native Anthropic
  adapter.

## Codex WebSocket lineage

Only `CodexFamily` requests with WebSocket transport and a non-empty
`SessionKey` can send `previous_response_id` or `generate:false`.

- Keep lineage runtime-only and keyed by the session pool entry.
- Install lineage only after clean `response.completed` with a non-empty ID.
- Bind completion callbacks to the current connection generation.
- Compare every context-bearing property before projecting an input suffix.
- Match `prior input + prior assistant output` before sending the suffix.
- Keep the complete request immutable for mismatch and HTTP fallback.
- Recover only an immediate first-frame `previous_response_not_found` once.
- Send that recovery as the complete request on the same socket.
- Invalidate later, repeated, partial, failed, canceled, or truncated lineage.
- Never log, persist, or export a response ID as projection metadata.

`generate:false` prewarm can accept empty input. Ordinary requests cannot.
Prewarm emits no provider events. `Prewarm` must return promptly when its context
is canceled; the engine cannot terminate a callback that ignores cancellation.

Completed WebSocket streams can report `RequestMetadata` as `full` or
`incremental`. Report complete and sent item counts without response IDs. Keep
OpenAI usage provider-reported: subtract `cached_tokens` from inclusive input
and expose the cached subset as `CacheReadTokens`.

## Native OpenAI Responses endpoints

A configured provider with type `"openai"` builds the Responses adapter under
that provider-map key. Require `base_url` for a non-built-in family.

Keep `Client.Family` equal to the configured family. The family is both the
router name and the opaque-data isolation boundary. Do not replay encrypted
reasoning between two Responses endpoints.

`responses_path` is valid only for an entry that builds this adapter.

## Stable request bytes

Prompt caches depend on stable bytes, not set equality. Keep tool order, system
segment order, message merge behavior, and JSON field behavior deterministic.

A test for cache-sensitive data must compare ordered wire bytes or ordered
decoded objects. A membership assertion is insufficient.

## Tests

- Test shared behavior through each affected real adapter.
- Use provider-contract oracles that do not call production normalization.
- Cover malformed HTTP responses and mid-stream errors.
- Assert unknown optional fields are omitted, not emitted as empty strings,
  when the contract requires omission.
- Never make a live provider call in the ordinary unit suite.
