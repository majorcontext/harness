# Provider

Read the root AGENTS.md. Read `message/AGENTS.md` for canonical data rules.

- Each adapter builds a new wire request from canonical history. Store no wire state.
- Every transcoder calls `message.NormalizeForWire` and reads `ToolResult.SafeContent`.
- Apply `imageclamp.Clamp`. Map tool-call IDs deterministically.
- Replay opaque `ProviderData` only for the matching family.
- Keep prompt-cache markers out of history. Keep request bytes stable and ordered.
- Classify errors with typed `provider.Error`. No caller matches error text.
- A stream that ends without a terminal event is `RetryableStreamTruncated`.
- `message.EffortUnset` sends no control. It is not `message.EffortOff`.
- Reasoning-history stripping differs by adapter. Never use one shared `!Reasoning()` check.
- Never replay encrypted reasoning between two Responses endpoints. `Client.Family` is the boundary.
- `Request.SessionKey` is the routing hint. Omit empty keys.
- Anthropic cache TTL is `"1h"` or `"5m"`. Reject other values.
- Send `previous_response_id` or `generate:false` only for `CodexFamily` over WebSocket with a `SessionKey`.
- Install lineage only after a clean `response.completed`. Never log or persist a response ID.
- Recover a first-frame chain miss once, with the complete request on a fresh connection.
- Add no static model-name vision list.
- Test each shared behavior through every affected real adapter. Compare ordered wire bytes.
- Cover mid-stream errors and malformed responses. Make no live call in unit tests.
