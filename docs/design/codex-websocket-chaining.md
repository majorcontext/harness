# Codex WebSocket response chaining and startup prewarm

## Status

Implemented design. This document describes the shipped Codex-family behavior
and its engine startup boundary.

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
6. Expose non-secret request-projection metrics and provider-reported cache use.

## Non-goals

- Do not enable chaining for the generic OpenAI provider or other endpoints.
- Do not persist remote response lineage.
- Do not send `previous_response_id` over HTTP.
- Do not set `store:true`.
- Do not add ChatGPT routing headers in this change.
- Do not change compaction output or canonical message storage.
- Do not infer compatibility from model names.

## Scope gate

The remote transport feature applies only when all conditions hold:

- `Client.Family` resolves to `CodexFamily`.
- `Client.UseWebSocketTransport` is true.
- `provider.Request.SessionKey` is non-empty.

The existing WebSocket configuration is the feature gate. No new user-facing
configuration controls response chaining or remote prewarm. A native Responses
client under another family never sends `previous_response_id` or `generate`.

The engine's local scheduling gate is the optional `StartupPrewarmer` interface.
`*openai.Client` implements that interface for every Responses family, then its
`Prewarm` method applies the Codex, WebSocket, and session-key checks above.
Consequently, fresh non-Codex Responses sessions can perform early local
discovery, hooks, and request assembly before `Prewarm` returns without network
activity. This is part of the shipped disclosure boundary, not a claim that
non-Codex requests use response chaining.

## Architecture

Harness keeps the complete logical request at the existing engine-to-provider
boundary. The OpenAI adapter owns wire compression through response chaining.

Each Codex WebSocket pool entry adds runtime-only lineage state:

- The previous complete logical `apiRequest`.
- The previous completed response ID.
- The output items from that completed response.
- A connection generation that rejects stale completion callbacks.
- Prewarm completion and cancellation signals owned by the engine task.

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
completed local construction, the engine schedules one bounded background task
when its initially configured provider implements `StartupPrewarmer`. Managed
roots wait for adoption and task-tool installation. Children wait until their
final lineage, model, agent type, and tool restrictions exist. Loaded sessions
do not prewarm.

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

The first real native turn consumes the startup prewarm once before context
validation, cached discovery-error checks, compaction, or user-history mutation.

- If prewarm is ready and compatible, the request reuses its response ID and
  sends only the new user and runtime input.
- A dedicated 15-second startup-prewarm deadline covers instruction loading,
  Skill discovery, hooks, tool and MCP assembly, dialing, the `generate:false`
  send, and terminal completion.
- If prewarm is still running, the turn waits only for the unused part of that
  dedicated deadline. The five-minute WebSocket stream-idle timeout does not
  extend prewarm.
- If the turn context is canceled, the engine cancels and detaches prewarm, then
  returns the context error without appending user history.
- If prewarm fails or times out, the turn proceeds with the normal complete
  request.

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
Skill discovery, plugin hooks, MCP connection attempts, and request assembly can
begin after fresh-session creation and before the first prompt for any provider
that implements `StartupPrewarmer`. Provider network activity begins only if
that implementation performs it. The shipped OpenAI implementation performs
network prewarm only after the Codex scope gate passes.

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
miss. Recovery is intentionally narrower than an ordinary "before visible
output" check. Only a miss in the immediate first response frame can recover.
The adapter clears lineage and sends the complete request once on the same
socket. This recovery does not consume the engine's user-turn retry budget.

Any later chain miss invalidates the socket and does not use special recovery,
even when earlier frames contained no model-visible output. A miss after visible
output is a typed truncated-stream error. Another non-immediate or repeated miss
follows the normal provider error classification.

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

A completed WebSocket model call reports `provider.RequestMetadata` on its
`EventDone`. The engine copies these fields into `turn_metrics`:

- `request_mode`: `full` or `incremental`.
- `complete_input_items`.
- `sent_input_items`.
- `previous_response_used`.

HTTP calls and providers that omit request metadata omit all four metric fields.
A successful full-request recovery after `previous_response_not_found` reports
the final projection as `full` with `previous_response_used=false`; the shipped
metadata has no separate recovery flag.

A `generate:false` prewarm is not a model inference, user turn, assistant
message, or `turn_metrics` record. The implementation does not emit prewarm
status, duration, or age metrics.

Existing `response.completed` usage remains authoritative for token accounting.
The server reports inclusive `input_tokens` and
`input_tokens_details.cached_tokens`. The OpenAI adapter stores the cached
subset as cache-read tokens and the non-negative remainder as uncached input
tokens. These values are disjoint in `provider.Usage` and `turn_metrics`.
Chaining does not infer or synthesize cache metrics.

## Session and process lifecycle

The engine creates one 15-second context when it schedules prewarm. An
independent deadline observer detaches session ownership when that context ends,
even if the worker callback has not returned. The first prompt waits only for
that same boundary. Session removal and prompt cancellation also cancel and
detach an owned task.

`StartupPrewarmer.Prewarm` must return promptly after context cancellation. Go
cannot forcibly terminate an arbitrary in-process callback. A provider or
transitive dependency that ignores cancellation can therefore leave one
residual, unowned callback goroutine blocked after the engine has detached it.
The callback cannot delay the first prompt or retain the prewarm handle, but the
engine cannot guarantee its termination. Cancellation-compliant providers leave
no residual task.

No prewarm state enters the journal or snapshot. Archive, restart, hibernation,
and process loss discard it safely. Canonical history remains sufficient for a
complete request after recovery.

Child sessions use the same rule as root sessions. A child starts only after its
final model, provider, lineage, agent type, tool restrictions, and session ID
exist.

Goal evaluators and compaction summarizers do not run startup prewarm. Loaded
sessions do not restart it. Ordinary calls can use Codex response chaining only
when their serialized session pool has compatible live lineage.

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
- A compatible provider that obeys context cancellation leaves no owned prewarm
  task after session shutdown.
- A noncompliant callback is detached at the deadline and can remain as the
  documented residual unowned goroutine.

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

Validate behavior and the shipped metrics before broad rollout:

1. Confirm first-turn prewarm compatibility with a wire trace.
2. Compare `request_mode` rates after the first turn.
3. Compare `complete_input_items` with `sent_input_items`.
4. Inspect first-turn and later-turn time to first token.
5. Inspect provider-reported cache-read input.
6. Monitor provider errors and truncated-stream rate.

The shipped metrics do not identify prewarm outcomes or chain-miss recovery as
separate counters. Use a wire trace when those distinctions are required.

Rollback disables Responses WebSocket transport or reverts the adapter change.
Canonical history and journals require no migration or repair.
