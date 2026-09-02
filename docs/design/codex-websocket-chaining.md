# Codex WebSocket response chaining and startup prewarm

## Status

Approved design. This document specifies the Codex-family implementation only.

Reference behavior: `openai/codex` Responses WebSocket v2 as inspected on
2026-09-02. Harness keeps its canonical session and provider boundaries.

## Problem

Harness reuses one Responses WebSocket per live Codex session, but it sends the
complete transcoded history on every model call. A long session can send
hundreds of thousands of input tokens again for each tool round and user turn.

Harness already sends a stable `prompt_cache_key`. The API also reports cached
input tokens. Prompt caching can reduce provider computation, but it does not
remove request serialization, transfer, parsing, or repeated context assembly.

Current Codex uses `previous_response_id` with an input suffix. It also sends a
startup `generate:false` request before the first user prompt. Harness must port
both mechanisms without making remote response state authoritative.

## Goals

1. Send only appended input items after a compatible completed Codex response.
2. Prewarm the first Codex request prefix before the first user prompt.
3. Keep `store:false` and encrypted reasoning replay.
4. Preserve complete canonical history and stateless HTTP fallback.
5. Fall back to a full request on every uncertain lineage condition.
6. Expose non-secret metrics for prewarm and incremental request use.

## Non-goals

- Do not enable chaining for the generic OpenAI provider or other endpoints.
- Do not persist remote response lineage.
- Do not send `previous_response_id` over HTTP.
- Do not set `store:true`.
- Do not add ChatGPT routing headers in this change.
- Do not change compaction output or canonical message storage.
- Do not infer compatibility from model names.

## Scope gate

The feature applies only when all conditions hold:

- `Client.Family` resolves to `CodexFamily`.
- `Client.UseWebSocketTransport` is true.
- `provider.Request.SessionKey` is non-empty.
- The native Harness engine drives the session.

The existing WebSocket configuration is the feature gate. No new user-facing
configuration controls response chaining or prewarm.

A native Responses client under another family keeps its current request bytes
and behavior.

## Architecture

Harness keeps the complete logical request at the existing engine-to-provider
boundary. The OpenAI adapter owns wire compression through response chaining.

Each Codex WebSocket pool entry adds runtime-only lineage state:

- The previous complete logical `apiRequest`.
- The previous completed response ID.
- The output items from that completed response.
- A connection generation that rejects stale completion callbacks.
- Prewarm completion, cancellation, timing, and status state.

The engine does not store OpenAI wire objects. The session journal, snapshots,
and canonical messages do not change.

The provider package exposes an optional startup-prewarm capability. The engine
uses it only after it resolves a provider and confirms that capability. The
ordinary `Provider.Stream` contract remains unchanged for every provider.

The engine owns when startup prewarm begins and when the first real turn
consumes it. The Codex adapter owns the connection, request comparison, response
ID, and suffix request.

## Incremental request algorithm

Harness first builds and transcodes the complete request exactly as it does
without chaining. This complete body remains available for HTTP fallback.

Before a Codex WebSocket send, the adapter compares the new request with the
lineage state.

### Non-input property comparison

The adapter requires equality for every context-bearing request property:

- `model`
- `instructions`
- `tools`, including order and schema bytes
- `temperature`
- `top_p`
- `max_output_tokens`
- `reasoning`
- `store`
- `include`
- `service_tier`
- `prompt_cache_key`

The comparison is exhaustive over `apiRequest`. A later field addition must
make a deliberate reuse decision. Request-local transport metadata does not
participate when it cannot change referenced model context.

### Input-prefix comparison

The expected input prefix is:

```text
previous complete request input + previous completed response output items
```

The adapter compares this expected prefix with the new complete input in order.
It uses the same OpenAI transcoder to derive prior response output items from
the completed canonical assistant message. This preserves the request shape for
assistant text, function calls, encrypted reasoning, and item order without
putting wire objects into durable history.

The comparison must account for provider-only item metadata in the same way as
current Codex. Metadata that does not affect model-visible content cannot cause
a false mismatch. Every model-visible field must match.

If the properties and prefix match, the adapter sends:

```json
{
  "type": "response.create",
  "previous_response_id": "resp_...",
  "input": []
}
```

The `input` value contains only items after the matched prefix. It can be empty
for the first request after a complete `generate:false` prewarm.

If either comparison fails, the adapter sends the complete request without
`previous_response_id`. A successful full WebSocket response establishes a new
lineage baseline.

## Response completion

The adapter updates lineage only after a clean `response.completed` event.

A normal inference completion update contains:

- The response ID returned by the terminal response.
- The complete logical request that produced the response.
- The canonical assistant output retranscoded into ordered response items.
- The connection generation that produced the response.

A `generate:false` prewarm completion has no canonical assistant message. It
establishes lineage with the completed response ID, the complete warmup request,
and an explicitly empty response-output item list.

A callback from an older connection generation cannot replace newer lineage.
An incomplete, failed, canceled, or truncated response never becomes a lineage
baseline.

Harness continues to use the response ID as the canonical assistant message ID.
That existing durable ID is not enough to restore lineage after process loss.
Only live adapter state authorizes an incremental request.

## Startup prewarm

### Scheduling

`NewSession` remains non-blocking. After a fresh session has an ID and has
completed local construction, the engine schedules one bounded background
prewarm when the scope gate passes.

The prewarm task prepares the stable first-request prefix before any user input:

1. Load and cache project instructions.
2. Discover and cache the Agent Skills catalog.
3. Apply `chat.params` to resolve request parameters.
4. Resolve the provider.
5. Build the effective built-in, MCP, and plugin tool plan.
6. Build the ordered system segments.
7. Build an empty-input logical request.
8. Connect the Codex Responses WebSocket.
9. Send `response.create` with `generate:false`.
10. Wait for `response.completed`.
11. Retain the live client state in the session-keyed pool entry.

The prewarm request sends the same non-input request properties as a normal
request. It keeps `store:false` and requests encrypted reasoning content.

The OpenAI transcoder permits empty input only for this internal Codex prewarm.
An ordinary model request still rejects an empty transcodable message set.

### First-turn resolution

The first real native turn consumes the startup prewarm once.

- If prewarm is ready and compatible, the request reuses its response ID and
  sends only the new user and runtime input.
- A dedicated 15-second startup-prewarm deadline covers instruction loading,
  tool assembly, dialing, the `generate:false` send, and terminal completion.
- If prewarm is still running, the turn waits only for the unused part of that
  dedicated deadline. The five-minute WebSocket stream-idle timeout does not
  extend prewarm.
- If the turn context is canceled, the engine cancels prewarm.
- If prewarm fails, times out, or becomes incompatible, the turn proceeds with
  the normal full request.

Prewarm age starts when the engine schedules the background task. One dedicated
15-second deadline owns the complete task, not only the WebSocket dial. It also
bounds project discovery, hook execution, MCP connection, request send, and the
wait for `response.completed`. The first turn must not start a fresh timeout
after that deadline has mostly elapsed.

The first real request still performs normal assembly and validation. Prewarm
does not substitute its earlier view of configuration. The adapter's property
and prefix comparison is the final compatibility check.

### Prewarm request contents

Creating a fresh Codex session can send these values before the first user
prompt:

- Base system prompt segments.
- `append_system_prompt` segments.
- Project instructions from `AGENTS.md` or `AGENT.md`.
- The Agent Skills catalog and local paths.
- Tool names, descriptions, and schemas.
- A deferred MCP catalog.
- Model, effort, service tier, and output controls.
- The session ID as `prompt_cache_key`.

Prewarm sends no user prompt, transcript, tool result, or arbitrary project file
content beyond existing instruction and Skill catalog discovery.

This behavior deliberately changes Harness's lazy boundary. Project reads,
plugin hooks, MCP connection attempts, and Codex network activity can begin
after session creation and before the first prompt.

### Validation and discovery errors

Project-instruction and Skill discovery keep their current user-visible
semantics. Prewarm caches the same result that the first prompt would load.

A deterministic discovery error does not emit an asynchronous session error.
The first prompt reads the cached error and returns it before appending user
history, exactly as it does now.

Provider, authentication, hook, MCP, and transport failures make prewarm
unavailable. They do not fail the future user prompt. The normal request path
runs and reports its own result.

## Connection and lineage invalidation

The adapter clears lineage when any of these events occurs:

- The WebSocket closes or is replaced.
- The connection exceeds its configured lifetime.
- A send fails.
- Reading the first frame fails.
- A stream is closed before its clean terminal event.
- The response is incomplete, failed, canceled, or truncated.
- The request falls back to HTTP.
- The same pool entry receives concurrent use.
- Prewarm is canceled, fails, or times out.

A property or prefix mismatch does not require closing a healthy socket. The
adapter sends a full WebSocket request and replaces lineage after clean
completion.

A model switch, effort change, service-tier change, tool-plan change, system
change, compaction, history repair, or history rollback naturally causes a
property or prefix mismatch. The adapter does not need engine-specific
invalidation calls for those operations.

Process restart and session resume create an empty WebSocket pool. Harness sends
a full request until a new live lineage exists.

## HTTP fallback and retries

The complete logical request remains immutable while the adapter derives a
WebSocket suffix. HTTP always receives the complete body.

A dial, send, or first-frame failure follows the existing HTTP fallback path and
clears WebSocket lineage. A failure after the adapter returns a live stream
remains a typed truncated-stream error. Engine retry policy remains unchanged.

A retry can use lineage only when the prior attempt completed cleanly. A failed
attempt cannot update lineage. Partial model output and partial tool intent do
not enter the next incremental baseline.

The adapter classifies the Codex `previous_response_not_found` error as a chain
miss. If the error arrives before any model-visible output, the parser marks the
socket reusable but clears lineage. The adapter then sends the complete request
once on that socket. This special terminal classification is an explicit change
to the current pool rule that invalidates every `error` event. The recovery does
not consume the engine's user-turn retry budget.

If the chain-miss error arrives after visible output, the adapter invalidates the
socket and reports the existing truncated-stream error. A second chain-miss or
any error from the complete recovery request follows existing classification and
retry behavior.

## Concurrency

Harness continues to serialize normal work for one session. The WebSocket pool
still refuses to multiplex response streams.

If concurrent use reaches one pool entry, the competing request uses the
existing safe fallback path and invalidates that entry's lineage generation.
The in-flight completion cannot later re-arm stale lineage.

Prewarm and the first prompt coordinate through one completion signal and one
cancellation path. Tests use channels and deterministic clocks. They do not use
sleep-based synchronization.

## Observability

Harness records no response ID value in logs or metrics.

Add non-secret fields or counters for:

- Prewarm status: `started`, `ready`, `consumed`, `failed`, `timed_out`,
  `cancelled`, or `stale`.
- Prewarm duration.
- Prewarm age when the first turn resolves it.
- WebSocket request mode: `full`, `incremental`, or `prewarm`.
- Complete logical input-item count.
- Sent input-item count.
- Whether a previous response was used.
- Full-request recovery after `previous_response_not_found`.

A `generate:false` prewarm is not a model inference, user turn, assistant
message, or `turn_metrics` record.

Existing `response.completed` usage remains authoritative for token accounting.
The server reports `input_tokens_details.cached_tokens`; Harness continues to
store the disjoint uncached and cache-read counts on the completed assistant
message. Chaining does not synthesize cache metrics.

## Session and process lifecycle

A prewarm task has a fixed deadline and cannot outlive its session indefinitely.
Dropping or canceling a session cancels pending prewarm. A session that never
receives a prompt leaves no unbounded goroutine.

No prewarm state enters the journal or snapshot. Archive, restart, hibernation,
and process loss discard it safely. Canonical history remains sufficient for a
full request after recovery.

Child sessions use the same rule as root sessions. A child can prewarm only
after its final model, provider, tool restrictions, and session ID exist.

Goal evaluators and compaction summarizers do not run startup prewarm. They can
use ordinary Codex response chaining only when they use the same serialized
conversation pool and satisfy the prefix rules. Separate internal request
shapes normally force a full request.

## Testing

Implementation follows test-driven development.

### Provider wire tests

- A first full request omits `previous_response_id`.
- A matching next request sends the response ID and input suffix only.
- An empty suffix after prewarm is valid.
- Each non-input property mismatch sends a full request.
- A shorter, reordered, changed, or repaired input sends a full request.
- A successful full request establishes a new lineage.
- Generic OpenAI families never send `generate` or `previous_response_id`.
- HTTP fallback receives the original complete request bytes.
- `previous_response_not_found` causes one full-request recovery.

### Stream and pool tests

- Only `response.completed` updates lineage.
- Incomplete, failed, canceled, and truncated streams clear lineage.
- Connection replacement clears lineage.
- Stale-generation completion cannot update lineage.
- Concurrent use cannot re-arm stale lineage.
- Response text, function calls, reasoning, and ordering form the expected
  next-request prefix.

### Engine prewarm tests

- `NewSession` returns while prewarm is blocked.
- Prewarm sends `generate:false`, `store:false`, and no user input.
- The first prompt waits only for the remaining dedicated prewarm deadline.
- The dedicated deadline bounds discovery, tool assembly, dial, send, and
  terminal completion; the normal stream-idle timeout cannot extend it.
- Prompt cancellation cancels prewarm.
- A failed or timed-out prewarm falls back to a normal request.
- Instruction and Skill errors retain current first-prompt behavior.
- Prewarm invokes the effective tool plan and request hooks once per assembly.
- A changed first-turn property makes the prewarm stale and sends a full request.
- Prewarm emits no user turn, assistant message, usage, or `turn_metrics`.
- Session shutdown leaves no prewarm goroutine.

### Regression and race tests

Run narrow provider and engine tests with `-race`. Run the repository-wide race
suite before handoff because the change adds cross-goroutine session and pool
state.

## Documentation changes

Update these documents with implemented behavior:

- `docs/models-and-providers.md`: Codex lineage, scope, and `store:false`.
- `docs/engine-request-cycle.md`: startup prewarm and first-turn resolution.
- `provider/AGENTS.md`: Codex-only lineage and full-fallback invariants.
- `engine/AGENTS.md`: bounded prewarm ownership and no-turn accounting.

## Rollout

The existing Codex WebSocket switch controls rollout. Deploy the change only to
clients already configured for the Codex WebSocket endpoint.

Validate these metrics before broad rollout:

1. Prewarm completion and consumption rate.
2. Incremental-request rate after the first turn.
3. Full fallback and `previous_response_not_found` recovery rate.
4. First-turn and later-turn time to first token.
5. Cached-input share.
6. Provider errors and truncated-stream rate.

Rollback disables Responses WebSocket transport or reverts the adapter change.
Canonical history and journals require no migration or repair.
