# Models and providers

This document describes model switching, context windows, reasoning effort,
cache affinity, and provider configuration.

## Model switching

`Session.SetModel` swaps the MAIN session model for later requests. History
transcodes from scratch every request, so there is no migration step. Three
routes reach `SetModel`: the built-in `model` session tool, a per-request
`prompt_async` model override, and `POST /session/{id}/model`.

`SetModel` is the single event choke point. On a real change (never a no-op
set to the current model) it persists the durable `recModel` resume record
AND emits `EventModelChanged` (carrying the new model), both while holding
`s.mu` — the same persist-and-emit-under-`s.mu` shape `RegisterGoal` uses.
The server's `Publish` maps `EventModelChanged` to the durable `model`
journal record. Every swap route funnels through this ONE emit, so a swap
journals exactly once — the handlers never emit `model` themselves. `recModel`
is the resume record `LoadSession` restores; `EventModelChanged` is the
observability event. They are separate and both fire on one swap.

The `model` session tool (gated on `Config.ModelTool`) has two actions:
`status` reports the current model, the configured aliases, and the configured
provider names; `set {model}` resolves a one-level alias (from
`Config.ModelAliases`, which mirrors `config.Aliases` — the engine never
imports config), parses the ref, VALIDATES the provider is configured
(`s.cfg.Providers.For`), then calls `SetModel`. A `set` to an unconfigured
provider returns a tool error listing the valid aliases and provider names and
changes nothing. There is deliberately NO `clear` action — a session always
has a model. Scope is the MAIN model only; the goal-evaluator and subagent
models are untouched.

`Config.ModelTool` is on by default. Config key `model_tool` (a `*bool`,
default true — like `instructions`) lets a host opt OUT; `harness run`,
`harness serve`, and the server `mkCfg` all set it from
`config.ModelToolEnabled()`. This differs from `GoalTool`, which opts IN only
when an evaluator is configured.

`POST /session/{id}/model` is the network counterpart: a client/dashboard swap
decoupled from prompting, so it never claims the run slot (it applies even
while a turn is running, taking effect on the next request). It validates a
non-empty `{model}` and rejects an unconfigured provider (400), an unknown
session (404), or an empty model (400) — the same validation as the tool — then
calls `SetModel`. Aliases are not resolved at this endpoint; resolve them
client-side, as the CLI does.

## An unknown model's context window is a refusal, not a shrug

`modelmeta.ContextWindow` answers "how big is this model's context window".
When it does not recognize a ref, `resolveContextWindow`
(`engine/context_window.go`) used to fold that into the SAME answer a
deliberate opt-out produces: source `disabled`, `compaction_armed=false`, and
a session that started anyway and ran with **no context management at all** —
until it died with "context exhausted" instead of compacting. An unrecognized
model is not a state to degrade into; it is a configuration an operator has to
fix.

`resolveContextWindow` now REPORTS the miss (an error wrapping
`ErrUnknownContextWindow`, whose text always names the offending ref) instead
of swallowing it, and does not decide what to do about it.
`Config.RequireContextWindow` does, through the single policy point
`requiredContextWindowErr` — so the definition of a miss lives in one place
and the policy lives with the session that must honor it, and the ERROR log
line fires once per miss however it was reached.

Only a REGISTRY MISS refuses. Four ways to have no window stay legitimate and
silent: an explicit positive `ContextWindowTokens` (naming the window IS the
missing information, so it satisfies the requirement for any model), an
explicit NEGATIVE one (`contextWindowSourceOptOut` — a stated choice, told
apart from `disabled` precisely because `disabled` can mean "unrecognized"), a
ZERO model ref (nothing to look up; the refusal belongs to whatever later
names a model), and a model the registry KNOWS whose window is below
`minAutoContextWindowTokens` (a known model, not a gap).

The refusal is recorded at the earliest point of use and surfaced everywhere a
model starts being used: `newSession`, `SetModel`, and `LoadSession`'s
post-replay re-derive set `Session.contextWindowErr`; `ContextWindowErr()` lets
a create route refuse before the session is durable or resident; `CheckModel`
is `ModelSupported`'s sibling gate, called at the same three `SetModel` routes
BEFORE the swap so a rejected ref never reaches the durable `recModel` record;
and every `Prompt` returns it before touching history, the provider, or the
instructions read. A RESUME is deliberately not fatal: a session that cannot
load cannot be listed, read, or exported either, and an operator would lose the
transcript along with the ability to fix the config.

`Config.RequireContextWindow` false — the engine zero value — keeps the
pre-fix behavior, so a bare embedder-built `engine.Config` and every test in
the package are unaffected; the config/CLI layer supplies the product default
of TRUE (`context_window_required`), the same unset-versus-explicit split
`prompt_retries` uses.

## Reasoning effort

`message.Effort` is the unified, provider-agnostic reasoning-effort level:
`off`, `minimal`, `low`, `medium`, `high`, plus the zero value `EffortUnset`
(empty string) that sends NO control at all. It rides one `provider.Request`
field (`Request.Effort`), the same way `MaxTokens` does, and each adapter maps
it to that provider's own wire shape at transcode time — so an effort swap,
like a model swap, needs no migration step:

- `provider/anthropic` enables extended thinking with a `thinking.budget_tokens`
  budget (minimal 1024, low 4096, medium 8192, high 16384). The API requires
  `max_tokens > budget_tokens` and rejects an explicit `temperature`/`top_p`
  while thinking is on, so `transcodeRequest` bumps `max_tokens` above the
  budget and drops both. `off`/unset emit no `thinking` block.
- `provider/openai` (Responses) sets `reasoning.effort` to the level string
  (minimal/low/medium/high). `off`/unset omit the `reasoning` object.
- `provider/openaicompat` sets the top-level `reasoning_effort` string; a
  gateway (Bifrost) maps it to the upstream provider's own knob. A non-off
  level sends the level string; `EffortOff` sends the literal string `"off"`,
  not an omitted field — several gateway upstreams reason BY DEFAULT when
  the field is absent, so omitting it cannot express "disabled." Measured
  (2026-08-12): Fireworks kimi-k3 through Bifrost streamed a full reasoning
  block (266 chars) with the field absent, and zero reasoning content (0
  chars, 8 vs 133 completion tokens) with the literal `"off"` sent. Only
  `EffortUnset` omits the field, leaving the gateway/model default in force.
  It surfaces returned reasoning from EITHER wire field — Bifrost/DeepSeek
  `reasoning_content` or OpenRouter `reasoning` — as a `Reasoning` part; a
  gateway sends one field, never both.

`Effort` does NOT police which model accepts which level — that is a
provider-and-model fact the engine cannot know from the ref alone. The adapter
sends the requested level and the provider is the final judge. A caller that
must gate levels per model (a dashboard picker) holds its OWN mapping.

**Downgrade strip — DELIBERATELY asymmetric between the two reasoning
adapters.** A stored thinking block (anthropic) or reasoning item (openai
Responses) can be a transcode-time destructive drop (throwaway request, intact
record); a later reasoning-ON turn replays the part from the unchanged history.
A strip is ever needed because a stored block shipped while the request omits
the reasoning control can be rejected, and durable in history it 400s every
later turn — a permanent wedge. But WHEN each adapter strips differs, because
the two providers default differently:

- `provider/anthropic` strips whenever the request enables no reasoning
  (`off`/unset, or a swap to a non-reasoning model). This is safe: Claude emits
  NO thinking block unless the control is sent, so an unset turn carries none
  to preserve.
- `provider/openai` (Responses) strips ONLY on an EXPLICIT `off` (a genuine
  "reasoning disabled" intent), NEVER on `EffortUnset`. OpenAI reasoning models
  (gpt-5) reason BY DEFAULT, so an unset turn — the default of every `harness
  run`/`serve` session, since nothing sets `Config.Effort` — still produces
  encrypted reasoning items, and those items are REQUIRED for stateless
  (`Store:false`) multi-turn tool use. Stripping them on unset wedged every
  turn-2+ gpt-5 tool continuation; an unset session now replays them exactly as
  every pre-effort-control build did (`stripReasoning` in
  `provider/openai/transcode.go`, gated on `req.Effort == EffortOff`). So
  `unset != off` here — do NOT re-fold the openai strip back onto
  `!Reasoning()`. (Regression: NEP-5272 review of PR #117.) One residual the
  off-only strip cannot enforce: a `SetModel` swap to a NON-reasoning openai
  model (gpt-5 -> gpt-4o) at unset effort still replays the stored items — the
  same per-model gating punt the enable direction has, so the caller (a
  dashboard picker) clears/re-validates effort on a model swap, NOT this
  transcoder.

The reverse (ENABLE) direction — turning reasoning ON over a prior tool_use
that lacks a thinking block — stays a documented limitation, since a signed
thinking block cannot be synthesized (see `provider/anthropic/
transcode.go`).

`Session.SetEffort` is the single event choke point, mirroring `SetModel`
exactly. On a real change (never a no-op set to the current level) it persists
the durable `recEffort` resume record AND emits `EventEffortChanged`, both under
`s.mu`. The server's `Publish` maps `EventEffortChanged` to the durable `effort`
journal record. That record ALWAYS carries the `effort` field, even on a clear:
`server/journal.go`'s `Event.Effort` is a `*message.Effort` (the same
explicit-zero-vs-absent pattern `QueueLen` uses), so a clear to `EffortUnset`
renders as an explicit `"effort":""`, never a dropped key — "cleared to the
provider default" stays byte-distinguishable from a malformed record.
`LoadSession` restores the level: the create-time level rides
the session header record, and every later `SetEffort` writes a `recEffort`
record. `Session.Effort()` reads it back.

`POST /session/{id}/thinking` is the network counterpart: a client/dashboard
swap decoupled from prompting, so it never claims the run slot. It validates the
`{effort}` value with `message.ParseEffort` (400 on an unknown level), accepts
an empty string as "clear to provider default", and rejects an unknown session
(404). Unlike the model endpoint it has NO provider gate (see above). The
current level is read back on `GET /session/{id}` (`effort`), the same way the
current model is.

**Effort at the three request-build sites is NOT uniform, by design.** The
main turn (`streamTurn`, `engine/engine.go`) sends `s.Effort()` — the
session's current level, read fresh every request. The two internal
tool-less calls diverge from that and from each other (issue #124): the
goal-loop evaluator (`runEvaluator`, `engine/goal.go`) always pins
`EffortOff` — see `docs/goal-loop.md` — because it is a classifier the model
must answer in one line, and reasoning-by-default gateway models can burn
its 256-token budget before ever emitting a verdict. The compaction
summarizer (`runCompactionSummary`, `engine/compact.go`) instead inherits
`s.Effort()`, the same as the main turn, because summarization is a real
writing task that benefits from the session's own quality setting;
`EffortUnset` stays `EffortUnset` there. Do not fold these two internal
sites onto one shared rule — one is a classifier, the other is prose.
Known residual (not addressed by issue #124, filed as issue #126): a
non-off session effort can raise the summarizer's effective output cap
above `compactionMaxTokens` (the anthropic and openai adapters both bump
the cap for reasoning — up to ~20480 tokens at `EffortHigh`, versus the
documented 1024 cap), and openaicompat applies no cap floor at all, so a
reasoning-heavy summary can truncate silently — `runCompactionSummary` has
no `StopReason` guard to catch it. A raised cap also delivers less context
reduction from this call, at the layer whose own failure runs to a hard
overflow that clears an active goal. A second, related residual (issue
#127): the summarizer sends folded history containing `ToolCall` parts
from turns that ran with no thinking block, and a non-off level here
enables thinking over that same history — the documented ENABLE-direction
"thinking blocks expected before tool_use" reject case, just reached from
compaction instead of a live turn.

**The summarization request always ends in a trailing `RoleUser` message,
never the folded range's own last message verbatim** (2026-08-19 incident,
session `ses_jumpy-pizza`). `foldEnd` (`Session.Compact`) is the last
message before the next KEPT turn's leading `RoleUser` message — ordinarily
that folded turn's own final assistant reply, `RoleAssistant` — so sending
`folded` as `req.Messages` verbatim ordinarily ends the wire request in an
assistant-role message, which the Anthropic Messages API treats as
assistant message prefill; some models reject prefill outright (400
`invalid_request_error`, "This model does not support assistant message
prefill. The conversation must end with a user message."). `runCompactionSummary`
builds its request via `compactionRequestMessages`, which appends one
trailing `RoleUser` instruction message (`compactionInstructionText`) after
`folded`, unconditionally — never a conditional check on the folded range's
last role, since a `RoleTool` message (a `message.ResolveOrphanToolCalls`
synthetic repair, or an ordinary tool result) also wire-transcodes to
Anthropic's `"user"` role and would otherwise mask the same bug depending on
where a fold boundary happens to land, exactly as it did live (`keep_turns=8`
happened to succeed on the same session where `keep_turns=20` failed).

**An empty summary is a graceful no-op, never an error surfaced to the
caller.** A summarization call that completes without a transport/stream
error but returns no usable text (`errEmptyCompactionSummary`) is reported
by `Session.Compact` as the same `TurnsFolded == 0` "nothing worth folding"
shape the too-few-turns case above already uses — no history mutation, no
journal write, no error returned — though `EventCompactionFailed` still
fires so the attempt stays visible to anything tailing events, and the
call's real usage is still accumulated into cumulative `Usage()` (it was a
billed call even though it produced nothing — this accumulation is
live-only, not journaled, since no compact record exists for a skipped
fold). Before ever calling the provider, `Compact` also skips a fold range
whose entire content is a single earlier compaction's own summary message
(`isLoneExistingSummary`): re-summarizing an already-compressed summary with
nothing new alongside it has nothing to gain, and was the live incident's
concrete trigger (a small `keep_turns` landed a fold range dominated by a
prior summary). Do not conflate this with a REAL summarization failure
(rate limit, transient 5xx, a truncated stream, a range too large to
summarize) — those still abort with an error, per §2 "Failure handling" in
`docs/design/context-compaction.md`.

`CompactResult.SkipReason` names WHICH of the three `TurnsFolded == 0`
shapes occurred (`SkipReasonNotEnoughTurns`, `SkipReasonLoneExistingSummary`,
`SkipReasonSummarizerEmpty`) — they used to be wire-identical, which hid two
real defects (review follow-up on PR #136, Findings A/B/C, fixed before
merge):

- **Hysteresis must latch on `SkipReasonSummarizerEmpty`, never on the two
  free skip reasons.** `maybeAutoCompact` only armed its churn-guard
  hysteresis when `TurnsFolded > 0`. A summarizer that always returns empty
  therefore never latched it: every subsequent over-threshold turn
  re-triggered a full, billed summarization call, indefinitely, at full
  input price — the "free" no-op was actually a recurring-spend bug
  (Finding A). It now also latches when `SkipReason ==
  SkipReasonSummarizerEmpty`, since that reason DID cost a call; it must
  still NOT latch on `SkipReasonNotEnoughTurns`/`SkipReasonLoneExisting
  Summary` — both are free, and latching there would permanently disarm
  compaction for an over-threshold session that simply lacks enough turns
  yet, since the guard only clears once `LastUsage()` dips back under
  threshold.
- **`isLoneExistingSummary` gates on the summary message's `ID`, never on
  `CompactionSummaryBanner`'s text.** The banner is a display convention; a
  user-typed or pasted message that happens to start with the exact banner
  string is a genuine turn with real content, not a lone existing summary —
  matching on text alone false-positived on it, skipped it forever without
  ever calling the provider, and under the automatic trigger the session
  never compacted again (Finding B). Every compaction summary's `ID` is now
  minted with the `cmpsum_` prefix (`compactionSummaryIDTag`) instead of the
  ordinary `msg_` prefix every other message gets, and `isCompactionSummaryID`
  tests exactly that prefix — a structural, unforgeable marker of
  compaction origin, the same pattern `message.IsSyntheticOrphanID` already
  establishes for a different synthetic-message kind. No text-based
  fallback exists for a summary minted by an earlier pre-fix build of this
  same PR (still `msg_`-prefixed): the miss is bounded and self-healing —
  `Compact` just re-summarizes that one old-style range like any other real
  content, and the fresh summary it produces carries the new ID tag from
  then on.
- **The `skip_reason` field on `POST /session/{id}/compact`'s response**
  (`compactResponseJSON`, `server/handlers.go`) surfaces
  `CompactResult.SkipReason` directly, `omitempty` (absent on a real fold) —
  see `docs/design/context-compaction.md` §1 for the wire shape (Finding
  C).

## Session affinity (prompt-cache routing hint)

`provider.Request.SessionKey` carries a stable, opaque session identifier on
every request the engine builds. The same per-request struct also carries
`Effort`. The engine sets `SessionKey` to `Session.ID` for main-turn assembly,
including its startup-prewarm request, and at the two internal request sites:
`runEvaluator` (`engine/goal.go`, the goal-loop evaluator) and
`runCompactionSummary` (`engine/compact.go`, the compaction summarizer). The
field itself is never persisted; the value it carries (`Session.ID`) already
is, as the session's own identity.

Two adapters forward it, each to its own field, because each provider
documents its own affinity hint:

- `provider/openaicompat` sets the wire top-level `user` field. This is a
  generic chat-completions gateway adapter (fronting Bifrost, OpenRouter,
  and similar); `user` is the field a Fireworks-style backend behind such a
  gateway reads for routing. OpenAI itself has deprecated `user` on its own
  API in favor of `prompt_cache_key`/`safety_identifier` (see the next
  bullet), but that deprecation is OpenAI's, not the gateway's: the
  openaicompat route keeps sending `user` because `user` is the field the
  measured Bifrost/Fireworks path above actually reads. Do not "fix" this
  adapter by swapping in `prompt_cache_key` — that field is specific to
  OpenAI's own API, and the openaicompat adapter targets non-OpenAI
  backends behind a gateway, whose measured path reads `user`. Swapping it
  would silently drop the measured cache-affinity win. The adapter now sends
  `prompt_cache_key` ALONGSIDE `user`, set from the same `SessionKey`: a
  gateway fronts several upstream shapes, and an OpenAI-shaped upstream
  behind it reads `prompt_cache_key` while the measured Fireworks path reads
  `user`. Both fields carry the identical value, one extra field costs
  nothing, and an upstream that knows neither ignores both. Add, never swap
  — the rule above still binds. Config key `no_prompt_cache_key` on an
  `openai-compat` providers entry suppresses that ONE field for a strict
  self-hosted upstream that rejects an unknown top-level parameter; `user`
  keeps carrying the session key, so the opt-out never costs the measured
  affinity win. It is rejected on any entry that is not `openai-compat` —
  the native openai adapter always sends `prompt_cache_key`, its own
  documented field.
- `provider/openai` (Responses API) sets the wire top-level
  `prompt_cache_key` field — the Responses API's own documented routing/
  cache-affinity hint, distinct from `user`. OpenAI combines it with the
  request's prefix hash to raise the chance repeat requests land on the
  same cache-holding backend.

Both follow the same omit-on-empty rule: a non-empty `SessionKey` sets the
field; an empty key omits it entirely, never an empty string.
`provider/anthropic` ignores `SessionKey` — it already uses explicit
`cache_control` markers, so a routing hint would add nothing; a live probe
through Bifrost (2026-08-12) confirmed a 41k-token cache write followed by a
41k-token cache read on the very next turn with no `SessionKey` involved.

The reason `SessionKey` exists at all is measured, not theoretical: Fireworks
serverless prompt caching is prefix-based, automatic, and PER-REPLICA.
Without a routing hint, a re-sent request can land on a different replica
and miss its own prefix cache. A live probe through Bifrost (2026-08-12)
sent a byte-identical 150k-token prompt twice: with no `user` field, the
second call still read `cached_tokens=0` at 10.8s time-to-first-token; with
a stable `user` field, the second call read `cached_tokens=150,300` at 2.8s
time-to-first-token, through the same gateway. Stateless routes re-send the
whole history every request, so a long session on the openaicompat route (a
gateway to Fireworks kimi-k3 and similar models) pays full prefill on nearly
every turn without this hint.

## Codex WebSocket response chaining

`provider/openai` compresses compatible Codex WebSocket requests without
changing canonical history. The transport feature requires all three values:

- the resolved client family is `codex` (`openai.CodexFamily`);
- `Client.UseWebSocketTransport` is true; and
- `Request.SessionKey` is non-empty.

Other Responses families can use the configured WebSocket transport, but they
never send `previous_response_id` or `generate`. HTTP requests never send those
WebSocket-only fields. Harness always keeps `store:false` and includes encrypted
reasoning content, so a complete stateless request remains valid.

Each session-keyed WebSocket pool entry keeps runtime-only lineage: the prior
complete `apiRequest`, its non-empty completed response ID, retranscoded
assistant output items, and the connection generation. Only a clean
`response.completed` callback from the current generation installs lineage.
An incomplete, failed, canceled, truncated, replaced, or concurrently used
connection cannot install or restore it. Harness never writes this state to the
session log or a snapshot. A restart or resume therefore starts with a complete
request.

The adapter transcodes the complete logical request before it considers
chaining. It compares every context-bearing non-input property and then expects
this ordered prefix:

```text
prior complete request input + prior completed assistant output items
```

If that prefix matches, the adapter sends `previous_response_id` plus only the
remaining input suffix. JSON values compare semantically, so insignificant
object formatting does not force a complete request. A property change, prefix
change, missing lineage, stale generation, or empty response ID sends the
complete request without `previous_response_id`. The complete body remains
immutable and is also the body used for every HTTP fallback.

A dial, send, or first-frame transport failure clears lineage and uses the
existing HTTP fallback for that call. A chained request can also recover once
on the same socket when its immediate first frame reports
`previous_response_not_found`: the adapter clears lineage and sends the
complete request. It does not spend an engine retry. A later chain miss never
uses this recovery, even if earlier frames carried no visible output. A miss
after visible output is a truncated stream; other non-immediate or repeated
misses use the normal provider error path.

A completed WebSocket call attaches request projection metadata to
`provider.EventDone`: `request_mode` (`full` or `incremental`),
`complete_input_items`, `sent_input_items`, `previous_response_used`, and
`chain_recovered`. The engine copies those values into `turn_metrics`. A
successful immediate chain-miss retry reports `request_mode=full` and
`chain_recovered=true`. HTTP calls and providers that do not report projection
metadata omit these fields.

Token accounting remains provider-reported. OpenAI reports `input_tokens` with
`input_tokens_details.cached_tokens` included. The adapter stores the cached
subset as `CacheReadTokens` and the non-negative remainder as `InputTokens`, so
the fields are disjoint and their sum reconstructs the reported input total.
Response chaining does not infer or synthesize cache usage.

## Codex HTTP request compression

`provider/openai` compresses every Codex-family HTTP Responses body with zstd
level 3. This includes a direct HTTP request and the full-body HTTP fallback
after a WebSocket dial, send, or first-frame failure. The request sends
`Content-Encoding: zstd`; decompression reproduces the complete JSON body.

The adapter compresses only after the WebSocket path declines the request.
WebSocket `response.create` frames therefore remain ordinary JSON and do not use
this encoder. WebSocket compression is a separate protocol extension.

The family gate is strict. A generic native OpenAI Responses client remains
uncompressed because compatible third-party endpoints may not accept zstd
request bodies. The `github.com/klauspost/compress/zstd` encoder initializes
lazily, uses compression level 3, and is pooled for concurrent HTTP calls. Debug
logs report only compression duration and byte counts.

An encoder initialization failure aborts the HTTP request before any bytes are
sent. The adapter never labels uncompressed bytes with `Content-Encoding:
zstd`.

## Anthropic cache TTL (default 1 hour)

`provider/anthropic` marks two prompt-cache breakpoints on every request —
the last system block and the last content block of the final message — and
never stores a marker in the session log (`transcodeRequest`,
`provider/anthropic/transcode.go`). The marker's TTL defaults to the
EXTENDED 1-hour cache, not the API's own 5-minute default.

This is an opt-OUT default, and it changes the wire for an operator who
configures nothing: every anthropic request carries the beta header and
writes 1h entries. Two deployments must know it. A proxy that rejects an
unknown `anthropic-beta` value fails every request, and a workload of short
one-shot sessions pays the 2x incremental write premium with no later turn
to read the entry back. Both set `cache_ttl: "5m"`, which restores the
previous bytes exactly.

`Client.CacheTTL` selects it: `"5m"`, `"1h"`, or empty for
`DefaultCacheTTL` (`"1h"`). Config key `cache_ttl` on the NATIVE `anthropic`
providers entry sets it, and `cmd/harness`'s `registry` passes it to the
client. The value is validated twice, and both checks fail loudly rather
than fall back: `config.validateCacheTTL` rejects an unknown value, and
rejects `cache_ttl` on any entry that is not the native anthropic adapter —
matching on IDENTITY, the map key `anthropic` with no `type`, never on the
key alone, since an entry keyed `anthropic` but typed `openai-compat` builds
an openaicompat client that would never read the value. `anthropic.
resolveCacheTTL` then rejects an unknown value again at the first `Stream`
call, like a missing API key. A typo must never silently ship different
cache economics.

Wire shapes, by TTL:

- `"1h"` sends `cache_control: {"type":"ephemeral","ttl":"1h"}` on both
  breakpoints, plus the request header `anthropic-beta:
  extended-cache-ttl-2025-04-11`. That header is the documented gate for the
  extended TTL. Some endpoints no longer enforce the gate and accept the TTL
  without it. Harness sends it regardless, because an endpoint that DOES
  enforce it must not fail.
- `"5m"` sends `cache_control: {"type":"ephemeral"}` and NO beta header —
  byte-identical to a build with no TTL support at all. This is the escape
  hatch for a gateway that rejects an unknown beta.

The default is 1h because of cost. Cache READS price the same at both TTLs.
A 1h WRITE costs 2x base input where a 5m write costs 1.25x, and that
premium applies only to the INCREMENTAL tokens each turn adds to the prefix.
A 5m expiry on a mature session, by contrast, rewrites the WHOLE prefix —
the entire history, at full input price. One such miss costs more than the
1h write premium over hundreds of turns. Agentic sessions exceed 5 minutes
by construction: one build, one live probe, or one subagent runs longer than
the window, and a user reads an answer before sending the next turn. The
commit that introduced this default carries the measured evidence.

## A second native Responses provider

`provider/openai` speaks the OpenAI Responses API. Other vendors speak the
same wire at their own host, under their own request path. Two config
fields let a deployment reach one without new adapter code.

`Provider.Type` accepts `"openai"` (`config.TypeOpenAI`) under ANY providers
map key. The key becomes the provider family, routed by the first segment of
a `provider/model` ref, exactly like an `"openai-compat"` entry.
`cmd/harness`'s `registerOpenAIProviders` builds one `openai.Client` per such
entry. `base_url` is required: an arbitrary endpoint under a caller-chosen
key has no sensible built-in default, the same rule `"openai-compat"`
follows. The bare `openai` key with an empty type keeps its own built-in
default and is unchanged.

`Provider.ResponsesPath` (`responses_path`) sets `Client.ResponsesPath`, the
path appended to the base URL. Empty means `/v1/responses`, the path the
Responses API documents and the only path this adapter could reach before.
The field is valid ONLY on an entry that builds this adapter — the native
`openai` key with an empty type, or any key with type `"openai"`.
`config.validateResponsesPath` rejects it elsewhere, matching on IDENTITY
(`buildsResponsesAdapter`) rather than on the map key alone, for the reason
`validateCacheTTL` already documents: the key `openai` with type
`"openai-compat"` builds an openaicompat client that would never read the
value.

`openai.Client.Family` overrides the family key `Name()` reports AND the
`ProviderData` tag the transcoder reads and the stream writes. Empty means
the package `Family` constant, so every existing caller is unchanged;
`registerOpenAIProviders` sets it to the providers map key. The tag matters
beyond routing. A Responses reasoning item is opaque, usually ENCRYPTED, and
scoped to the endpoint that minted it, and history replays it verbatim on
every later request. One shared `"openai"` tag across two endpoints would
make the canonical family match succeed between them, so a session that
swapped models would replay one endpoint's ciphertext to the other. A
per-client family makes that a cross-family DROP instead — the canonical
crossing rule — which costs one turn of reasoning continuity and nothing
else.
