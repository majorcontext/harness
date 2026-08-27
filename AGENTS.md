# AGENTS.md

Instructions for AI coding agents working in this repository.

## Project Overview

Harness is a Go agent harness (in the spirit of pi and opencode) built around four priorities, in order:

1. **Speed** — especially startup speed. `harness --version` under ~5ms, TUI first frame under ~30ms. These are CI-enforced budgets, not aspirations.
2. **Extensibility** — a language-agnostic plugin protocol with a first-class Go SDK.
3. **Composability** — headless engine, event streams on stdout, client/server split, MCP in both directions.
4. **Dynamic model choice** — swap providers/models mid-session or per-subagent with zero migration cost.

## Architecture

The engine is a headless Go library; every frontend (CLI, TUI, server API) is a client.

```
cmd/harness        thin CLI: flags → engine or client
engine/            session loop, tool registry, event log
provider/          one adapter per API family (anthropic, openai-responses, gemini, openai-compat, bedrock)
message/           canonical message/part types + per-provider transcoders
plugin/            hook bus, JSON-RPC stdio protocol, plugin SDK
server/            HTTP+SSE / unix socket exposing the engine
tui/               a client, nothing more
```

### Core invariants

- **A session is an append-only log of typed events.** User messages, model deltas, tool calls, results, model switches — all events. UIs, JSON output, and plugins are subscribers to the same stream.
- **The session log stores the canonical message format, never a provider's.** Every request, the provider adapter transcodes canonical history → provider wire format from scratch (stateless transcoding). Mid-session model swap = next request uses a different transcoder. No migration step.
- **Provider-specific opaque data (reasoning/thinking blocks, encrypted reasoning items) is stored as provider-tagged attachments** on canonical messages: replayed verbatim to the same provider, dropped when crossing providers. Tool-call IDs are internal; each transcoder maps deterministically to provider-compliant IDs. Prompt-cache markers are injected at transcode time, never stored.
- **Model refs are `provider/model`** plus user-defined aliases (`fast`, `smart`) from config. The models.dev catalog snapshot is embedded at build time and refreshed async — never on the startup path.
- **A history repair that runs on live or persisted state is additive-only.** `LoadSession` writes the repaired slice back into live history, so a repair that deletes loses data permanently — not for one request, but for the life of the session. Add synthetic parts; never drop, reorder, or relocate a part another producer wrote. A transcode-time repair MAY be destructive, because it builds one throwaway request and never touches the record. Put every destructive rule on that side of the line. (Incident: a `ResolveOrphanToolCalls` rewrite deleted genuine tool output in three shapes and was reverted; see NEP-5293.) The concrete split is in "Wire normalization" below.
- **An empty tool result must never serialize as `null`.** The provider reads a null-content `tool_result` as ABSENT and rejects the whole request with "tool_use ids were found without tool_result blocks immediately after" — naming a block that IS in the payload. A tool that produces no output (a `grep` that matches nothing) is enough to wedge a session forever. `message.NoToolOutputText`, `ToolResult.SafeContent`, and `ToolResult.MarshalJSON` hold this line; every transcoder reads through `SafeContent`, never `Content`. (Incident: NEP-5272.)

### Wire normalization

Two functions repair `tool_use`/`tool_result` pairing. They sit on opposite
sides of the live-versus-transcode line in the invariant above.

`message.ResolveOrphanToolCalls` is the LIVE-path repair. `LoadSession`
applies it and writes the result back into history, so it stays purely
additive. It deliberately leaves several shapes wire-invalid. Do not "fix"
it — that is the whole point of the split.

`message.NormalizeForWire` (`message/wire_normalize.go`) is the
transcode-only sibling. Every transcoder calls it instead. It builds one
throwaway request, so it may relocate a part. It must still never delete a
real `ToolResult`.

`NormalizeForWire` closes four shapes `ResolveOrphanToolCalls` cannot:

1. Two `tool_use` blocks share one call ID in one assistant message.
2. A `ToolCall` sits in a non-assistant message.
3. A `ToolResult` precedes its `ToolCall`.
4. An intervening same-side message separates a `ToolResult` from its
   `ToolCall`. Every transcoder merges adjacent same-role messages (see
   `transcodeRequest`'s same-role merge, `provider/anthropic/transcode.go`),
   so the wire sees RUNS. `ResolveOrphanToolCalls` tests strict
   `messages[i+1]` and is blind to this.

Relocation is bounded. `computeRelocationBarrier` moves a result no later
than the origin run of the next real result. That keeps the original
relative order intact. A move that would break the bound is refused.

`message/wire_oracle_test.go` is the specification both functions are
tested against. Derive it from the provider contract only, never from
either function's internals. See the oracle rule under Testing.

### Ambient engine context is a structured, unforgeable part

The engine appends its own live status to the newest user message every
request — engine identity (`[engine: ...]`), managed-process status
(`[processes: ...]`), degraded-MCP status (`[mcp: ...]`), and the
parked-goal notice (`[goal: ...]`). This is a `message.EngineContext` part,
NOT a `Text` part. A bare `Text` block is byte-indistinguishable from
user-typed or pasted text, so a payload a user pastes that contains
`[engine: ...]` once inherited the same trust the engine's own block
carries — a trust-spoofing surface. `EngineContext` is a distinct part-kind
only `withAmbientStatus` (`engine/process.go`) produces, so a user- or
paste-authored part is always a `Text` and can never BE one, however its
bytes are shaped. Every transcoder renders an `EngineContext` through
`message.RenderEngineContext`, which wraps the block in the
`message.EngineContextOpenTag`/`EngineContextCloseTag` sentinel, and renders
every `Text` through `message.NeutralizeEngineContextSentinel`, which
defangs any literal sentinel that text carries. Only a genuine
`EngineContext` can therefore emit the sentinel on the wire, so the base
system prompt (`cmd/harness`, `ambientContextGuidance`) tells the model to
trust the sentinel-wrapped block and to distrust bracketed text outside it.
The render stays an ordinary text block on every provider — no new wire
feature. `EngineContext` is runtime-only (appended to the throwaway
per-request copy, never the durable log — the prompt-cache-prefix and
never-persisted rules below are unchanged) but still round-trips through the
canonical JSON union like every other part. Never revert this to a `Text`
part, and never make the guidance trust bracketed text syntax again. (Fix:
the NEP ambient trust-spoofing finding; superseded PR #113's prose-only
stopgap.)

### Project instructions (AGENTS.md)

The engine auto-injects a project's `AGENTS.md` into the system prompt. On the
first `Prompt` of a session (never at `NewSession` — the startup budget rule)
it walks up from `Config.WorkDir` for `AGENTS.md` (falling back to `AGENT.md`),
stopping at the git root or filesystem root; the closest file wins, per the
[agents.md](https://agents.md/) convention. The file is schema-less Markdown —
no headings are required or parsed. The segment is appended after
`Config.System` and before hook (`system.transform`) segments, cached for the
session, and never written to the session log (loaded fresh on resume).

A present-but-unusable file (invalid UTF-8, or empty/whitespace-only) fails the
first `Prompt` — a project that meant to supply instructions must not run
silently without them. A missing file is fine. Oversize files are truncated at
64 KiB with a marker. Disable with `-no-instructions`, config `instructions:
false`, or point at a specific file with config `instructions_path`.

### Agent Skills

The engine advertises [Agent Skills](https://agentskills.io) in the system
prompt following the spec's progressive-disclosure model. On the first `Prompt`
(alongside instructions loading, same load-once-cache-error pattern) it runs
`skill.Discover` over each configured directory, merges the results sorted by
name, and injects one system segment **after** the instructions segment and
before hook (`system.transform`) segments. That segment is stage 1 only: a
header telling the model it MUST read a skill's `SKILL.md` with the `read_file`
tool before relying on it, then one line per skill — `name — description (path:
<abs SKILL.md>)`. Stage 2 (the body) is deferred to that read.

`Config.SkillsDirs` selects the directories: nil (the default) uses
`<WorkDir>/.agents/skills` when it exists; an explicit empty slice disables
discovery. A malformed `SKILL.md` or a duplicate skill name across dirs fails
the first `Prompt` loudly (same semantics as a malformed AGENTS.md). Skills are
never written to the session log — a resumed session rediscovers them. Config
`skills_dirs` (array; a non-empty project value overrides the user value
entirely) and the repeatable `-skills-dir` run/serve flag drive it.

### read_file image support

The built-in `read_file` tool (`engine/filetools.go`) can return an image
file as real visual content, not mangled text. `readPathContent` opens the
target path exactly once and classifies it by its magic bytes
(`http.DetectContentType` over at most the first 512 bytes) — never by its
extension: a `.txt` file that is actually a PNG is still recognized as an
image, and a `.png` file that is actually text stays a text read. On a
recognized image (`image/png`, `image/jpeg`, `image/gif`, `image/webp`),
`read_file` returns a `message.ToolResult` whose Content is `[Text, Blob]`:
a one-line Text summary (format, byte size, and pixel dimensions) followed
by a `message.Blob` carrying the real file bytes. This is the same
`Text`+`Blob` shape MCP's `mcpContentToParts` already produces
(`engine/mcp.go`) — `read_file` is a second producer of it — so every
transcoder's existing Blob handling and the imageclamp dimension/byte-size
pass (`imageclamp.Clamp`, called from every transcoder's
`transcodeRequest`) apply with no new wiring. `read_file` never bypasses
that clamp: it does not resize, re-encode, or otherwise touch pixels
itself. Because `imageclamp.Clamp` runs later, at transcode time, an image
it downscales or re-encodes can end up described by dimensions or a byte
size that no longer match the summary `read_file` reported when it read
the file; this is a known, accepted mismatch, not a defect to fix in
`read_file` itself.

**Only the Anthropic route puts a tool-result image on the wire.**
`imageclamp.Limits.RecurseToolResults` is true for `provider/anthropic`
only; `provider/openai` and `provider/openaicompat` set it false and
instead replace a tool-result Blob with a text note,
`"[N image attachment(s) omitted]"` (`toolResultOutput`,
`provider/openai/transcode.go` and `provider/openaicompat/transcode.go`).
This is pre-existing wire-format behavior `read_file` inherits, not
something this feature introduces, but it means a `read_file` image reaches
the model as pixels only on the Anthropic route; on the other two the model
sees only the one-line Text summary.

`readPathContent` applies three guards on the image path, in order:

1. The sniff read uses `io.ReadFull`, not a single `Read`, so a short
   `read(2)` — realistic on a pipe or FUSE mount — never misclassifies a
   real image as plain text.
2. The read is bounded at `readFileMaxImageBytes` (20MB), checked
   against an `io.LimitReader` over the same open handle, never against a
   separately captured `os.Stat` size a concurrently growing file could
   outrun. This cap is separate from and smaller than any provider's own
   wire limit, which `imageclamp.Clamp` enforces at transcode time; it
   exists only so `read_file` itself never loads an unbounded file into
   memory. An over-cap image returns a plain text error and no Blob.
3. The body must decode with `image.DecodeConfig` before `read_file`
   commits to the image outcome. A corrupt or truncated file that merely
   opens with a matching magic-byte prefix fails this check; `read_file`
   then reads the true remainder of the file (unbounded, same handle) and
   returns it as ordinary text instead of shipping a Blob the model cannot
   use. This guard is not airtight for GIF: the `GIF87a`/`GIF89a` header
   carries no checksum, so text that happens to start with those exact six
   bytes still "decodes" with fabricated dimensions. A real file colliding
   with that prefix is vanishingly unlikely; this is a documented, accepted
   residual.

A non-image binary file (sniffed as `application/octet-stream` or similar)
keeps `read_file`'s existing (unbounded) text-read behavior; `readPathContent`
still reads it exactly once, through the same handle its sniff already
opened.

**Known gap, filed as issue #129**: a transcode-time degrade of an image
Blob to a text placeholder for a model with no vision capability is not
implemented. No per-model vision-capability signal exists anywhere in the
codebase to gate it on — the embedded models.dev catalog this file's own
"Architecture" section describes as a design goal is not yet built, and
`provider.Request` carries no capability flag comparable to `Effort` or
`SessionKey` that a caller could set from one. Building this now would mean
inventing an ad hoc, likely-wrong static model list, so it is deferred to
issue #129. Until it lands, a model with no vision support receives the
image Blob exactly as any vision-capable model does; how it handles that
block is between the model and its provider.

### write_file read-before-overwrite guard

`write_file` (`engine/filetools.go`) refuses to overwrite an EXISTING file
the session has not read. Before this guard, `write_file` overwrote any
existing file unconditionally — a model could destroy a file it never
opened, with no recovery path. `edit_file` never had this hole: its
`old_string` match is exact-content-required by construction, so it cannot
blindly clobber unseen content. Claude Code and opencode both close the
same gap on their own write tools (opencode's is a literal `"You must read
file X before overwriting it"` error); this guard gives `write_file` the
same property.

`Session.recordRead`/`readHashFor` (`engine/filetools.go`) track, per live
session and in memory only, every path `read_file` has read or
`write_file`/`edit_file` has written, keyed on the RESOLVED absolute path
(`s.resolvePath`'s output, never the raw tool argument — two different
relative arguments that resolve to the same file must not be tracked as two
separate paths), mapped to the sha256 hash of that path's raw on-disk bytes
at the moment of that read or write. `read_file` hashes the complete raw
file bytes it already read off disk for its own content classification
(`readPathContent`'s `TextData`/`ImageData`) — never the offset/limit-sliced
text it returns to the model. This matches the reference guards (Claude
Code, opencode): the guard authorizes per successful OPEN, not per byte
displayed — a windowed read of a large file authorizes replacing the whole
file. The hash is recorded only at a `read_file` return that hands the
model content; a read that errors (offset past end-of-file) records
nothing.

`write_file` on a path that `os.Stat` resolves to an existing regular file
requires, in order: (1) the path is present in this session's read set —
absent means `write_file: %s exists and has not been read this session;
read it first (or use edit_file)`; (2) hashing the path's CURRENT on-disk
bytes fresh (never trusting the recorded hash's age) matches the recorded
hash — a mismatch means `write_file: %s changed on disk since it was read;
read it again before overwriting`. A path that does not exist
(`fs.ErrNotExist`) is unguarded — creation is `write_file`'s main job, and
there is no prior content to protect. Any OTHER stat failure (permission,
transient metadata error) refuses the write with `write_file: cannot stat
%s to check the read-before-overwrite guard` — a failed stat cannot prove
no protected file exists there. A
successful `write_file` or `edit_file` records/updates the written path's
hash to the new content's hash, so a write immediately followed by another
write to the SAME path (the assistant overwriting its own just-written
content, or an `edit_file` followed by a `write_file` on the same path)
never spuriously re-triggers the guard — the session already knows exactly
what is on disk because it just put it there.

The read set is runtime-only: never persisted, never folded by
`LoadSession`. A reloaded session starts with an EMPTY read set, so a
resumed session must `read_file` a path again before `write_file` can
overwrite it — even a path the session genuinely read in a prior process
life. This is deliberately conservative and matches the guard's purpose:
the guard exists to stop an overwrite of content the model never actually
saw THIS session, and a resumed model has no live memory of raw file bytes
from a prior process either, only whatever text happened to land in the
persisted transcript. The read set lives on `Session` state, not `Config`,
so `configSnapshot` (used to seed a spawned child's config) never copies
it — a spawned child starts with its own empty read set, correctly, since
it is a different session that has read nothing yet.

Tool calls execute strictly sequentially today
(`Session.runToolCalls`/`runToolCall`, `engine/engine.go`), so the read
set's own `sync.Mutex`-guarded map access is sufficient protection now.
A future parallel tool executor must serialize concurrent
`write_file`/`edit_file` calls against the SAME resolved path — matching
`edit_file`'s existing same-path safety requirement — rather than relying
on the map's per-operation lock alone, which does not cover the
check-current-hash-then-write sequence as one atomic unit against a
concurrent writer to the same path.

`bash` writes (a model redirecting output to an existing file via a shell
command) are explicitly OUT of scope: harness cannot classify an arbitrary
shell command as a file write versus anything else it might do, so this
guard covers only the two built-in tools that make a structured, typed
claim to write a file.

### Base loop retry

The base interactive `Prompt` loop retries a transient provider error itself,
so a plain box prompt never surfaces a one-off HTTP 500. `streamTurnWithRetry`
(`engine/prompt_retry.go`) wraps `streamTurn` at its single call site in
`runAgenticLoop` (`engine/engine.go`). It retries only when the error is
classified retryable through `provider.AsRetryable` — `server_error`,
`overloaded`, `rate_limited`, or `stream_truncated`, never by matching error
text — AND the budget has an attempt left. Every other error returns on the
first attempt with ZERO retries: a `context.Canceled` abort, an
`*interruptedTurnError` (whose partial `runAgenticLoop` must still append —
retrying would duplicate the model's already-emitted tool intent), a
`provider.AsPermanent` malformed-request shape, or any deterministic failure.
The final surfaced error still emits one `session.error` and drops the usage
exactly as before; an intermediate masked attempt emits neither. A masked
attempt is still a full `streamTurn`, so it DOES bump the per-session turn
counter (`s.turn`, reported by `session_info`) and re-fire the per-request
hooks (`chat.params`, `system.transform`) and `OnRequest` — one bump and one
hook pass per attempt, exactly like the goal loop's per-attempt behavior. Only
the `session.error` and usage are suppressed for a masked attempt.

One class is retry-eligible WITHOUT `provider.AsRetryable`: a completed but
EMPTY turn (no non-empty text, no tool call — e.g. thinking consumed the
whole `max_tokens` ceiling; see `emptyTurnError`). Two deliberate deviations
from the masked-attempt rules above. First, a discarded empty attempt's
usage IS accumulated into cumulative `Usage()` (it was a fully billed
completion, unlike a transport failure — same principle as the empty
compaction summary), while `lastUsage` is left alone. Second, the nesting
math: an empty turn that survives all `PromptRetries+1` attempts surfaces a
deterministic error, which goal mode's worker tier retries
`goalWorkerRetries` more times — worst case `(PromptRetries+1) *
(goalWorkerRetries+1)` = 9 fully-billed calls — and then STOPS the goal
with the empty-turn reason. Before this class existed the same turn was a
silent success and a goal limped on with nothing appended; halting with a
legible reason is the intended trade. The fail-fast for the deterministic
`max_tokens`-exhaustion shape (cutting the 9 to 3) is a filed follow-up on
the PR that introduced this.

Retrying `streamTurn` is idempotent for history and tool side effects:
`streamTurn` makes ONE model call and never executes a tool (`runAgenticLoop`
runs tools only AFTER `streamTurn` returns a `StopToolUse` message), so a
failed attempt ran no side effect to redo. The one shape that DID emit tool
intent before failing arrives as `*interruptedTurnError` and is excluded.

The emit stream is NOT idempotent, so `streamTurnWithRetry` closes that gap.
A failed attempt can emit `EventTextDelta`/`EventReasoningDelta` for partial
text before its stream dies, and the retry re-streams that text from scratch.
`streamTurnWithRetry` emits one `EventTurnRestart` (`engine.go`) before each
retry, so a subscriber that renders deltas incrementally drops the stale
partial and rebuilds it from the retry — never the two runs concatenated
(`Hello wor` then `Hello world` shown as `Hello worHello world`). The server
forwards `EventTurnRestart` live over SSE (`server/journal.go`'s `Publish`);
the turn's final `EventMessage` still reconciles history regardless.

`Config.PromptRetries` bounds it: additional attempts, zero (the engine zero
value) DISABLES retry, config/CLI default 2 via `config.Config`'s `*int`
`prompt_retries` key (`PromptRetriesValue`). The backoff
(`basePromptRetryDelay`: 1s, then 2s, `time.NewTimer`) is deliberately SMALLER
and SHORTER than the goal loop's tiers below — an interactive user waits on
the turn, so this smooths a blip in a second or two, never the goal loop's
~30min weather schedule (`promptTurnWithRetry`, `goal.go`). The two are
distinct: the base loop wraps ONE model call and is inherently idempotent; the
goal loop's `promptTurnWithRetry` wraps a whole worker turn (with the
tool-executed non-idempotency gate) and parks on exhaustion.

The two also NEST. A goal worker turn runs through `s.Prompt`/
`s.runAgenticLoop` (`goal.go`), so every one of `promptTurnWithRetry`'s outer
attempts now issues up to `1+PromptRetries` inner `streamTurn` calls. For a
persistent retryable condition the worst case is `goalRetryableMaxAttempts`
(12) times `1+PromptRetries` (3) — about 36 full-input-price model calls,
where the goal-loop tiers alone assume ~12. This is deliberate: the fast inner
budget (1s, then 2s) smooths a one-off blip inside a single worker turn before
the outer weather tier ever counts it, so a goal loop rides a brief provider
blip without spending an outer attempt. `PromptRetries` 0 disables the inner
budget for a host that wants the outer tiers to be the only retry.

### Max-tokens auto-continue

A turn whose stop reason is `provider.StopMaxTokens` means the provider cut
the model off mid-emission — it did not choose to stop. Before this existed,
`runAgenticLoop`'s `if stop != provider.StopToolUse` branch
(`engine/engine.go`) treated every non-`tool_use` stop reason alike: append
`asst`, synthesize an is_error result for any orphaned `ToolCall` part via
`appendUnexecutedToolCallResults` (NEP-5272, see "Wire normalization"
above), and return. For `max_tokens` specifically that return settles the
session idle with nothing further ever prompting it. Incident: box
harness-parallel-tools — the model emitted a large tool call, the provider
stopped mid-emission with `max_tokens`, the engine synthesized the
unexecuted-call result exactly as designed, and the session then sat idle
until a human noticed and re-prompted it. On an autonomous fleet a silent
work stoppage is as bad as a crash.

`runAgenticLoop` now branches on `stop == provider.StopMaxTokens` inside
that same `if`: `maybeAutoContinueMaxTokens` decides whether to `continue`
the loop (issuing a real follow-up model call in this SAME `Prompt` call)
instead of returning. This applies identically whether or not `asst` carried
a `ToolCall` part — a pure-text `max_tokens` truncation (Claude Code's own
behavior is to let the turn end and rely on the user re-prompting) gets the
same auto-continue an autonomous harness session needs, not a human-facing
half-answer.

**A genuinely mid-emission tool call keeps its identity; only its Arguments
are cleared.** A cross-model adversarial review of this PR raised a
CRITICAL finding: a `StopMaxTokens` turn's trailing `ToolCall` can carry
non-empty but syntactically invalid `Arguments` — the raw, truncated
`partial_json` Anthropic's `assembledBlock.toolCall`
(`provider/anthropic/anthropic.go`) leaves behind when `max_tokens` lands
before the block's own `content_block_stop`, e.g. `{"comm` — and claimed
replaying that into the continuation request fails `json.Marshal` before it
reaches the provider. That premise does NOT hold: `message.Message.Normalize`
(`Session.append`'s `appendWithUsage`, run on every append) already coerces
the identical invalid-Arguments shape to nil in place, through the SAME
`*ToolCall` pointer `asst.Parts` already holds — so by the time the
continuation request is built, `Arguments` is already safe. This is not
incidental; it is the deliberate, incident-tested fix for a real production
defect (two goal sessions dead at "json: error calling MarshalJSON... "; see
`TestPersistTruncatedToolCallArguments`, `engine/tool_call_poison_test.go`).
The finding is REBUTTED WITH EVIDENCE, not implemented: no code change. See
`TestMaxTokensPartialJSONMarshalsThroughRealTranscoder`
(`engine/max_tokens_wire_test.go`), which drives a genuinely truncated
`partial_json` tool call through a REAL `anthropic.Client` (`provider/anthropic`,
via an `httptest` server, not a hand-rolled stand-in) and proves the
continuation request the client actually sends decodes cleanly server-side,
with the truncated call's identity preserved and its `Arguments` cleared —
this is the test that pins the rebuttal and must go red before any future
"drop the call entirely" change lands unchallenged.

**The continuation nudge is a genuine new turn, never assistant prefill.**
The follow-up call carries the synthetic unexecuted-tool-call result (for
any COMPLETE call the engine chose not to execute) plus a one-shot nudge —
`s.pendingContinuationNudge`/`continuationNudgeSegment`. Unlike every other
ambient status segment (process, MCP, goal-parked, identity, task
notifications — see "Ambient engine context" above), this one is NOT glued
onto an existing message via `withAmbientStatus`:
`appendContinuationNudgeMessage` appends a genuine NEW `message.RoleUser`
message, carrying the nudge as its own `*message.EngineContext` part, to the
END of `streamTurn`'s own throwaway per-request message copy.
`withAmbientStatus` scans backward for the newest EXISTING `RoleUser`
message, which by the time a continuation request is built is an EARLIER
message than the just-truncated assistant turn (and its synthetic tool
result, if any) — leaving the canonical request ending in `RoleAssistant` or
`RoleTool`. Anthropic serializes that as assistant PREFILL: some models
reject it with a permanent 400, and even an accepting model sees a
"continue" instruction that chronologically precedes the output it refers
to. `appendContinuationNudgeMessage` never touches `s.history` — same as
every ambient segment — so a session reload, or any later unrelated
request, never sees the nudge. It still rides every attempt a
transient-error retry makes for that ONE follow-up call (mirroring
`checkoutTaskNotificationsSegment`'s idempotent-reread shape,
`taskdelivery.go`) and is cleared by `runAgenticLoop` the instant that whole
`streamTurnWithRetry` call returns, so it never bleeds into a later,
unrelated turn.

**Queued operator input is drained before every continuation, not only at
tool-call boundaries.** `drainQueuedPromptsIntoHistory` (`engine/engine.go`)
is the shared implementation behind the tool-call-boundary drain (after a
`StopToolUse` round actually runs a tool) and the max_tokens continuation
branch (right before looping back for another follow-up call) — both are
points where `runAgenticLoop` is about to issue another provider request in
the SAME `Prompt` call, so both are valid mid-turn steering opportunities.
An operator prompt queued while a long truncated response streams is
delivered on the very next continuation request, not left undelivered for
the whole continuation chain.

Loop safety is the critical part: `runAgenticLoop`'s local `maxTokensUsed`
is a PER-PROMPT BUDGET, spent by every continuation issued in the loop and
NEVER reset — including across an intervening `StopToolUse` round. (An
earlier version of this counter, `maxTokensStreak`, DID reset on any
non-`max_tokens` stop, which let a model alternate `max_tokens` and
`tool_use` — including denied, unknown, or failing tool calls, none of
which touch `toolExecCount` — indefinitely inside one `Prompt` call,
spending an unbounded number of continuations without ever tripping
`Config.MaxTokensContinuations`.) `maxTokensUsed` is bounded by
`Config.MaxTokensContinuations`: `Config.MaxTokensContinuations+1`
max_tokens stops used within the loop trips the bound.
`maybeAutoContinueMaxTokens` then returns a
`*maxTokensContinuationExhaustedError` naming the bound, wrapped
`provider.MarkPermanent`, instead of arming yet another doomed attempt;
`runAgenticLoop` emits `session.error` and returns it, the same "honest
terminal, never a silent success" shape `emptyTurnError`'s own budget
exhaustion uses (see "Base loop retry" above).

**A goal-loop retry must never re-run an already-exhausted continuation
chain.** With the default bound of 3, one worker attempt that exhausts the
budget already makes 4 completed, fully billed `max_tokens` calls before
`*maxTokensContinuationExhaustedError` is even returned.
`maybeAutoContinueMaxTokens` wraps every value of that type
`provider.MarkPermanent` at its one construction site, so
`promptTurnWithRetry`'s existing `provider.AsPermanent` fail-fast branch
(`engine/goal.go`) stops after that ONE attempt instead of re-running the
whole exhausted chain up to `goalWorkerRetries` (2) additional times — which
would otherwise multiply 4 calls into 12 for one goal boundary. Like every
other permanent-classified worker error, this PARKS the goal (stays
resumable) rather than clearing it: the condition that produced the
exhaustion might not recur on a later resume.

`Config.MaxTokensContinuations` follows `PromptRetries`'s own
unset-vs-zero config idiom: the engine field's zero value DISABLES
auto-continue entirely (a bare embedder-built `engine.Config`, and every
test that constructs one directly, keeps the exact pre-fix behavior — the
turn ends immediately on the first `max_tokens` stop). The config/CLI layer
(`config.Config.MaxTokensContinuations *int`, key
`max_tokens_continuations`, resolved via `MaxTokensContinuationsValue`)
supplies the product default of 3; an explicit `0` disables it the same way
`prompt_retries: 0` disables base-loop retry.

A task child runs its turn through this exact same `runAgenticLoop` — a
child `Session` is a full `NewSession(childCfg)`
(`SessionManager.Spawn`, `engine/session_manager.go`), and `childCfg` comes
from `configSnapshot`, a whole-struct copy of the parent's `engine.Config`
— so `MaxTokensContinuations` (and every other engine.Config field) reaches
a child with no separate wiring. A child that hits `max_tokens`
auto-continues under the identical bound the root does.

### Per-turn metrics

`streamTurn` (`engine/engine.go`) emits one structured `turn_metrics` line per
COMPLETED model call — a stream that reached `EventDone`; a turn that errors
or is interrupted mid-stream (see `interruptedTurnError` above) reports
nothing, since there is no finished call to summarize. This is the box-fleet
answer to "why does this session feel slow": TTFT and stream duration, token
and prompt-cache accounting, and request shape, greppable straight off a
process's stderr.

Fields: `session_id`, `model` (full `provider/model` ref), `ttft_ms` (elapsed
from just before `prov.Stream` to the first non-`EventActivity` stream event
— `EventActivity` carries no content, so a keep-alive ping or an in-progress
tool-argument chunk must never be mistaken for "first byte"; if `EventDone`
itself is the first event, `ttft_ms` covers the whole call and `stream_ms` is
0), `stream_ms` (first delta to `EventDone`), `input_tokens`/`output_tokens`/
`cache_read_tokens`/`cache_write_tokens` (passed through from
`provider.Usage` verbatim), `system_len`/`tools_count`, and `retry` (the
1-indexed attempt number `streamTurnWithRetry` — `engine/prompt_retry.go` —
was on when this call completed; 1 for a turn that succeeded on its first
try). `system_len` is computed identically to the server's `request.meta`
record (`len(strings.Join(req.System, "\n"))`, see `server/journal.go`'s
`OnRequest`) — deliberately, not coincidentally: `session_id` + `model` +
`system_len` together are a natural join key between a `turn_metrics` stderr
line and the durable `request.meta` record for the same request, with no new
ID threaded through the provider boundary.

`Config.OnTurnMetrics func(TurnMetrics)` is the seam. Unlike every other
`On*` callback in `Config` (`OnEvent`, `OnRequest`, `OnStorePhase`), nil is
NOT "disabled": `emitTurnMetrics` substitutes `defaultTurnMetricsLog`
(`engine/turn_metrics.go`), a `slog.NewJSONHandler` line written to
`os.Stderr` — the same stream every other structured log line in this repo
uses (see `cmd/harness/main.go`'s "Structured logging: JSON to stderr"
comment). Stderr keeps the line out of `harness run`'s stdout, which is
the model's answer channel, while a deployment's log pipeline (Kubernetes
captures both streams) scrapes it identically. A plain `harness run`/`harness serve` process
with no embedder wiring therefore still emits this line by default; an
embedder that wants a different sink (an OTel exporter, an in-memory test
recorder) sets `OnTurnMetrics` and never needs to suppress the default
first.

`Config.Now func() time.Time` is the clock this measurement reads (nil
resolves to `time.Now` in `newSession`), scoped to this one seam rather than
a general engine clock — every other timestamp in the package still reads
`time.Now` directly. It exists so a test can script an exact instant sequence
instead of depending on real elapsed wall-clock time between two in-process
calls with nothing to wait on between them, per the Testing rule against real
sleeps.

This was built for a deployment that ships a served process's stderr to a
log pipeline (a fleet of boxes running `harness serve`, each pod's stderr
collected by a Vector-style agent into BetterStack or an equivalent log
store). The intended query there filters `msg: "turn_metrics"` and groups by
`model`/`session_id` to compare TTFT and stream-duration distributions across
sessions — quantifying, with real numbers instead of a feeling, whether a
session "feels slow" because of provider latency, prompt-cache misses, or
something else entirely.

### Goal loop

`Session.PursueGoal(ctx, condition, GoalOptions)` drives the ordinary `Prompt`
loop toward a natural-language completion condition. Turn 1 prompts the raw
condition; after **every** turn an independent, TOOL-LESS evaluator model
(`GoalOptions.Evaluator`, resolved through the same `Config.Providers` registry,
`MaxTokens` 256) is asked to answer `MET: <reason>` / `NOT MET: <reason>`
(parsed leniently). The evaluator request always pins `message.EffortOff`
(`runEvaluator`, `engine/goal.go`) — it is a classifier, not a reasoning task,
and it never inherits the session's own effort level. On openaicompat,
`EffortOff` sends the literal `"off"`; on anthropic, it emits no thinking
block — both routes now spend none of the evaluator's 256-token budget on
reasoning. (Issue #124.) The openai Responses route is a known residual:
`reasoningEffort` (`provider/openai/transcode.go`) omits the `reasoning`
object for `EffortOff` exactly as it does for `EffortUnset`, and a
gpt-5-class model reasons by default with no adapter-level way to disable
it — so an evaluator on that route can still spend its budget on reasoning.
A NOT MET verdict re-prompts
with a fixed-template guidance message carrying the reason; MET returns
`Achieved`. `MaxTurns` (0 = unlimited) bounds it. Evaluation is advisory: a
retryable-class provider error from the
evaluator call rides the matching in-boundary backoff before the boundary
counts as failed — the long weather-tier schedule
(`goalRetryableMaxAttempts`, ~30min) for `overloaded`/`rate_limited`/
`server_error`, or the short stream-truncation tier
(`goalStreamTruncatedMaxAttempts`, 3 attempts, ~5s) for a stream cut before
its terminal event — `runEvaluatorWithRetry` mirrors `promptTurnWithRetry`'s
own per-class split exactly (see below); two unparseable replies in a row
(the second re-asked with a stricter prompt) or a non-retryable provider
error also fail the boundary immediately. A failed boundary no longer
clears the goal — it journals a durable `goal.eval_failed` record (carrying the consecutive
failure count), substitutes a fixed evaluation-unavailable notice for the next
turn's guidance in place of the evaluator's text, and `continue`s: the worker
keeps working. Any later boundary that DOES parse a verdict (MET or NOT MET)
resets the consecutive count to zero — the horizon is a streak, not a
lifetime total. Only after `goalEvalFailureLimit` (5) consecutive failed
boundaries does the loop treat the evaluator as durably broken: it clears the
goal with a dedicated reason, and the server maps that terminal to a
`session.error` plus a distinct `turn.end outcome=evaluator_exhausted` — loud
and machine-distinguishable, since every failure below the horizon is
deliberately silent apart from the journaled record.
Durable `goal.set` / `goal.eval` / `goal.eval_failed` / `goal.parked` /
`goal.achieved` / `goal.cleared` records land in the session log, so
`LoadSession` restores an active goal (condition only; counters reset) via
`Session.ActiveGoal()` — resume never auto-runs it, the caller decides. The
loop also emits `goal.*` engine events so the server journals them. Config
`goal_evaluator_model` supplies the evaluator for `harness run -goal` and
`POST /session/{id}/goal`.

The evaluator's own `CONVERSATION TRANSCRIPT` field is BOUNDED, independent
of whatever context window the MAIN session model has and independent of
whether automatic compaction (below) has fired at all.
`renderConversationBounded` (`engine/goal.go`, called from `runEvaluator`)
replaced the old unconditional `renderConversation(s.History())`: box
bx-01m0x8996, a real long session, died with "engine: goal evaluator failed
at 5 consecutive turn boundaries: context exhausted: prompt 245332 tokens >
limit ..." because the evaluator's prompt grows with the WHOLE session
transcript forever — unlike the main session, which automatic compaction
protects, the evaluator had no bound of its own at all. The budget comes
from `goalEvaluatorTranscriptBudgetBytes`, which resolves the EVALUATOR
model's own context window via `modelContextWindowLookup`
(`modelmeta.ContextWindow`) — the same table automatic compaction's
`resolveContextWindow` (`engine/context_window.go`) reads, called
DIRECTLY rather than through `resolveContextWindow` itself: that
function's `minAutoContextWindowTokens` floor answers "should automatic
compaction ARM for this window," so a real, small, KNOWN window (gpt-4's
documented 8,192 tokens) reports identically to a genuinely UNRECOGNIZED
model (0, disabled) — conflating them would give a real small-window
evaluator a budget roughly double its actual limit, the exact overflow
class this fix closes. `goalEvaluatorTranscriptBudgetBytes` trusts ANY
positive, known window from the table, however small, and falls back to
`goalEvaluatorFallbackContextWindowTokens` (mirroring
`minAutoContextWindowTokens`'s value) only when the model has NO entry at
all. It also reserves headroom for the system prompt and MaxTokens' output
budget, and applies a conservative fraction (`goalEvaluatorContextBudgetFraction`,
0.5) on top of the same crude ~4-bytes-per-token estimate
(`bytesPerTokenEstimate`) automatic compaction's own resilience fallback
uses — reused, not reinvented.
`renderConversationBounded` walks history from the NEWEST message backward,
accumulating rendered blocks until the budget would be exceeded, and
prefers "summary + tail" for free rather than summarizing a second time:
Compact (`engine/compact.go`) already splices its own summary message in
place of whatever range it folded, tagged with the `compactionSummaryIDTag`
prefix, so the backward walk simply STOPS the instant it includes such a
message — everything before it is already captured there. The newest
message is always kept regardless of budget (an empty transcript can never
be assessed); a truncated transcript is prefixed with
`goalEvaluatorTruncationNotice` so the evaluator, and an operator reading a
later `goal.eval` record, never mistakes a bounded view for the whole
session.

A worker-turn error (`s.Prompt` failing) is retried by `promptTurnWithRetry`
on one of FOUR independent budgets, chosen by classification via
`provider.AsRetryable` — never by matching error text.

One class skips every budget. Before it selects a budget,
`promptTurnWithRetry` tests `provider.AsPermanent` — its fail-fast check
(`engine/goal.go`) — and fails fast: a permanent error gets ONE attempt and
no retry.
`provider.MarkPermanent` marks a malformed request shape. The anthropic
adapter applies it to an HTTP 400 `invalid_request_error`, and to the same
error type mid-stream (`provider/anthropic/anthropic.go:114` and `:484`),
only after `parseContextOverflow` rules overflow out — the two are disjoint.
A retry never repairs a malformed request, and each attempt costs a full
turn at full input price. A permanent error still PARKS, exactly like every
budget exhaustion; it never clears. `permanent` is threaded through only to
select a more accurate classified reason and tier name
(`classifyGoalWorkerError`, `goalWorkerParkedError`), so an operator can
tell a single-attempt park from `goalWorkerRetries`+1 identical attempts.

One shape is wrapped `provider.MarkPermanent` by the adapter but is
DELIBERATELY EXCLUDED from this fail-fast branch: `provider.AsProviderExhausted`
— an ACCOUNT-level usage/quota wall (PR #174's `provider.ErrKindProviderExhausted`,
originally added for task-child resumability; see `engine/session_manager.go`'s
`FailKindProviderExhausted`). An adapter marks it permanent for ordinary
HTTP-retry purposes (no short backoff schedule outlives a monthly quota),
but a wall lifts on its own, unchanged, so treating it as a doomed malformed
request silently kills goal supervision on the very first usage-limit
rejection. Live evidence: box bx-01m0x8996 parked after "1 permanent-tier
attempt(s)" on "You have reached your specified API usage limits" and never
resumed without an operator `DELETE` + re-register. `promptTurnWithRetry`
and `PursueGoal`'s worker-turn handling both check
`provider.AsProviderExhausted` explicitly and fold a positive result into
their local `retryable`/`class` bookkeeping (`class` set to the dedicated
marker `goalClassProviderExhausted`, never one of `provider.RetryableClass`'s
real values) — reusing the existing stall/park recording machinery rather
than adding a fourth field throughout.

A deterministic
failure (not classified retryable, not permanent) gets `goalWorkerRetries` (2) additional
attempts on the short schedule (~5s total: 1s, then 4s). A provider error
classified `overloaded`/`rate_limited`/`server_error` gets a separately
budgeted `goalRetryableMaxAttempts` (12) backoff (~30min total, jittered, 5s
doubling to a 5min cap) that never spends the deterministic budget. A
provider error classified `provider.RetryableStreamTruncated` — a response
stream that died before its terminal event, with no HTTP status or inline
error to classify from (see the idle-stream watchdog below) — gets its own
`goalStreamTruncatedMaxAttempts` (3) budget on the SAME short schedule the
deterministic tier uses (~5s total): truncation is retryable, but it is not
weather — waiting longer never raises a stream ceiling, and every retry
re-prompts a full turn at full input cost — so it must ride neither the fast
deterministic budget nor the long weather-tier one. A `provider.AsProviderExhausted`
failure gets its OWN `goalProviderExhaustedMaxAttempts` budget (equal in
size to `goalRetryableMaxAttempts`, on the identical jittered schedule) —
never the ordinary weather counter, so a concurrent overload spell and an
account wall in the same turn can never silently share or steal from one
another's budget. It deliberately rides the SAME schedule ordinary weather
uses rather than computing a wait from the provider's own `RecoverHint`
("you regain access on <date>"): `RecoverHint`'s format varies by provider
and by plan (see `provider.Error.RecoverHint`'s doc comment), so it is
never parsed into a duration, only ever quoted verbatim to a model-visible
caller. A wall that clears within the budget (a burst rate limit that
reached this classification, or a short-lived cap) resumes the worker turn
— and the whole goal — with NO operator action; a wall measured in hours or
days still exhausts the budget and parks, but honestly classified via
`classifyGoalWorkerError`'s dedicated branch ("provider account usage limit
exhausted the retry budget"), never as a permanent, unretriable request.
Every attempt records a
`goal.stalled` record regardless of tier, so the loop is never silent.
Exhausting ANY of the four budgets — or the non-idempotency gate stopping
retries early once a tool has already executed this attempt — PARKS the goal
instead of clearing it: `PursueGoal` exits, journals a durable, CLASSIFIED
`goal.parked` record (never raw provider error text — the same leak rule
`goal.eval_failed` follows), and returns a distinct `*goalWorkerParkedError`
sentinel (`engine.IsGoalWorkerParked`) WITHOUT calling `clearGoal` —
`goalActive` stays true, the condition is untouched, generation-gated exactly
like `goal.stalled`/`goal.eval_failed` so a park racing a concurrent
`UpdateGoal` is silently discarded rather than attributed to a condition the
model never saw. This supersedes both this package's earlier
deterministic-tier clear and GitHub issue #61's in-loop retryable-tier
self-re-arming `continue` — the latter pinned the run slot to the parked loop
for the whole outage; exiting instead frees the slot, so a queued prompt
dispatches as an ordinary turn during a long outage instead of only ever
being injected mid-turn into a doomed attempt. Context overflow (issue #62)
is the one deliberate exception and still clears immediately, never parks:
no amount of waiting fixes an oversized request, so parking it would just be
a slower-burning zombie instead of a fix. Parking has no streak horizon
(unlike the evaluator's 5-boundary terminal above) — every exhaustion parks
immediately, and `DELETE /session/{id}/goal` remains the only clear path for
a parked goal.

Each retry re-issues the SAME directive, and `Prompt` appends whatever text
it gets as a brand-new user message — it has no notion of "this is a retry,
do not duplicate." Left alone, N failed attempts leave N unanswered copies of
one directive, and every LATER request pays for all of them. `Prompt`
persists each copy before the provider call that fails, so the duplicates
reach the durable log, not just live history.

`promptTurnWithRetry` therefore never appends a second copy for the common
case. It tracks one `anchorID`, naming the point right before this turn's
CURRENT, still-unanswered directive — starting as `lastMessageID`, captured
once before attempt 1 — then dispatches each retry one of three ways
(`engine/goal.go`, `tailAfterAnchor` shares the anchor-to-tail lookup; see
docs/design/goal-retry-directive-reuse.md):

- Attempt 1 calls `Prompt`, which appends the directive.
- A retry whose tail after `anchorID` is EXACTLY the previous attempt's
  unanswered directive (`directiveReuseEligible`) calls `runAgenticLoop`
  instead. That runs the turn loop against history as it stands and appends
  nothing, so the existing message is answered rather than duplicated.
- Any other tail falls back to `dropUnansweredDirective` plus `Prompt`, then
  re-anchors: `anchorID` moves to `lastMessageID`, the point right before
  the fresh directive `Prompt` is about to append. A later attempt's reuse
  check then measures from that new directive, never from the turn's
  original start.

`runAgenticLoop` is `Prompt`'s own loop body, split out unchanged
(`engine/engine.go`). `Prompt` still appends and then calls it, so `Prompt`'s
observable behavior is identical: same events, same `emitStatus`, same usage
accounting. Note that `maybeAutoCompact` stays in `Prompt` and does NOT run
on the reuse path. That is deliberate, and the reason is that history did
not grow: the reuse path is reachable only when the tail is exactly one
message, so no new completed turn appeared to fold since attempt 1 already
ran the check. (`maybeAutoCompact` folds only COMPLETED turns, so it would
never have folded the unanswered tail directive itself.) One narrow
residual: history sitting right at the threshold, where appending a
directive would tip it over, no longer triggers a mid-outage fold. That is
accepted — the outage that piles up retries is also when the summarizer's
own provider call fails, and compaction is best-effort anyway.

`dropUnansweredDirective` remains the fallback for the interrupted-turn tail
(the directive plus a partial assistant message and its synthetic
tool-result message), and for any tail a denied tool call or delivered mail
makes undroppable. It anchors on a message ID, never on a history length,
and `isSafeToDropDirectiveTail` approves only that interrupted-turn shape
and the bare directive. Any other tail is left untouched — a denied tool's
result, or an already-delivered "OPERATOR MESSAGES" block, must never be
discarded. It mutates only live history and can never retract a journaled
record, which is why the reuse path above, not a retraction, is what keeps
the log clean. `promptTurnWithRetry`'s re-anchor above bounds an undroppable
residue's cost to ONE extra duplicate directive for the rest of the turn,
never one per remaining attempt: re-anchoring past it lets reuse resume on
the very next attempt instead of re-appending against a tail that can never
shrink back to a droppable shape again.

An idle provider stream — one that goes silent with no bytes, no
`EventDone`, no error, ever — is bounded by a per-request idle-stream
watchdog (`engine/stream_watchdog.go`, `Config.StreamIdleTimeout`, config key
`stream_idle_timeout_s`): every stream event resets its timer, and on expiry
it cancels the request's child context and converts the resulting
cancellation into a classified `provider.RetryableStreamTruncated` error
instead of an anonymous "context canceled" — this is what feeds the
stream-truncation tier above. It defaults to 5 minutes (mirroring Codex's
`stream_idle_timeout_ms`), a negative value disables it, and it guards the
worker turn, the goal evaluator, and the compaction summarizer's streams
alike (`armIdleWatchdog` wraps all three, so a silent stream at any of them
can no longer wedge the session forever while holding the run slot).

Automatic compaction's over-threshold check
(`maybeAutoCompact`/`estimatePromptTokensFromHistory`, `engine/compact.go`)
has its own resilience fallback: a provider route that reports all-zero
input usage on a turn that DID complete is treated as missing data, never as
"0 tokens, never over" — the check falls back to a crude ~4-bytes-per-token
estimate walked from the actual session history so the overflow-prevention
layer keeps functioning instead of going permanently dark on that route,
which otherwise runs to a hard context overflow that clears (never parks) an
active goal.

`Config.ContextWindowTokens` — the size that gates automatic compaction at
all — is resolved by `newSession` (`engine/context_window.go`,
`resolveContextWindow`), not just read verbatim from whatever the embedder
passed in. Precedence: an explicit, positive `Config.ContextWindowTokens`
always wins and is pinned for the session's lifetime (`contextWindowExplicit`
on `Session`, set once at construction); otherwise the session's MODEL is
looked up in package `modelmeta` — a curated, static table of
`provider/model` -> context-window tokens sourced from models.dev's
`limit.context` field (bifrost's `/v1/models` was investigated first and
ruled out: it returns the bare OpenAI listing shape with no context-length
data at all). A model-derived value under `minAutoContextWindowTokens`
(16k) is treated as bogus metadata and ignored — logged, never armed. An
unrecognized model (or no metadata at all) leaves compaction disabled,
identical to the field's original zero-value behavior. `SetModel` re-runs
the same derivation against the new model whenever the window wasn't
explicitly pinned, so a mid-session model switch keeps the window matched
to whichever model is actually running — switching FROM a recognized model
TO an unrecognized one disarms compaction again, not just leaves the old
window in place. One INFO log line (`"engine: context window"`) fires at
session start and on every switch that changes the effective window, naming
the resolved tokens and source (`config`/`model-derived`/`disabled`) — the
operator signal for "is compaction armed and why," added after a box
(`jumpy-pizza`) died with a raw `context exhausted: prompt N tokens > limit
M` provider error because `ContextWindowTokens` was opt-in and the boxes
platform set it nowhere. See docs/design/context-compaction.md's "Where
`ContextWindowTokens` comes from" addendum for the full incident writeup.

On the server, a worker-parked sentinel maps to `session.error` plus a
distinct `turn.end outcome=worker_parked`, and `goalTracker` folds the
durable `goal.parked` record into a third `paused` arm (`pause_reason:
"worker_failure"`, alongside the existing boot-only `"restart"` and live
`"provider-backoff"`) — `compositeState` forces `idle` for it, and for a
restart pause, unless a turn is actually running, which reads `busy`: forced
idle must never mask a live turn (an ordinary prompt, or the resume prompt
that eventually re-arms the goal, can be streaming while the goal itself
sits parked), whereas provider-backoff's loop is merely waiting and keeps
reading `goal-running` regardless of whether a turn happens to be running.
Resume needs no new machinery: the existing activity-driven
`maybeAutoArmGoal` re-arms any active goal — parked or not — the next time an
ordinary prompt turn completes, resetting the `worker_failure` presentation;
`runGoal`'s own tail deliberately never auto-arms (the same anti-churn
property that already stops a freshly-parked goal from immediately
respawning a loop against an empty queue).

`harness serve` can also make this turn/goal lifecycle visible on stderr:
`server.Options.Logger`, when set (`cmd/harness/main.go` wires a
`slog.NewJSONHandler(os.Stderr, nil)` logger into it for `serveCmd`), emits a
structured line at every `recordTurnEnd` call (INFO for outcome "completed",
WARN otherwise) and at the `goal.*`/`session.error` durable-record choke
points — a heartbeat for the life of the box instead of logging only at
boot/config/MCP wiring, matching Codex's own structured stream-retry
logging. Nil (the default) disables all of it; every call site nil-guards
first, so an unset Logger is exactly the prior silent behavior.

A worker-parked goal is also surfaced in-session, model-facing:
`Session.goalParked` (set when a park lands, cleared at every `PursueGoal`
entry) drives a third ambient status segment — alongside the process and MCP
segments — appended to the newest user message of any turn that is NOT
itself one of this loop's own worker turns, naming the classified reason and
stating the goal resumes automatically. It is runtime-only and never
persisted; after a process restart, visibility reverts entirely to the
boot-only `goal.paused`/`pause_reason: "restart"` presentation instead — a
deliberate, documented asymmetry.

The condition itself is adjustable mid-loop. `Session.UpdateGoal` rewrites an
active goal's condition, journals a durable `goal.updated` record, and emits
`EventGoalUpdated` — same lock-and-emit-under-`s.mu` shape as `RegisterGoal`;
a same-condition update is a silent no-op, updating an inactive goal errors.
`PursueGoal` takes a per-turn snapshot (condition, a runtime-only generation
counter, active) instead of closing over the original parameter, so a live
loop picks up new text at its very next turn boundary — both the worker
directive and the evaluator call. The generation counter guards stale
verdicts: if `UpdateGoal` lands while an evaluator call for generation N is
in flight, a MET (or stalled) verdict for N is discarded on return — no
`goal.achieved`, no `goal.eval`, the loop just continues against the new
condition, never a false-positive completion against text the model never
saw. `ClearGoal` is unaffected — it keys on `goalActive`, not condition
equality, so it still stops the loop at every point it does today.

A built-in `goal` session tool (gated on `Config.GoalTool`) lets the model
inspect or drive its own goal in-process: no HTTP round-trip, no run-slot
claim. `status` reports `{active, condition}`; `set` arms a new goal via
`RegisterGoal` (errors telling the model to use `adjust` if a goal is already
active); `adjust` rewrites an active goal's condition via `UpdateGoal`. There
is deliberately **no `clear` action** — see below.

`Config.GoalTool` is on whenever `goal_evaluator_model` is configured, in
`harness run` and `harness serve` alike, entirely independent of the `-goal`
flag — a plain `harness run -p ...` with that config set still registers the
tool. But what happens after `set`/`adjust` differs by host: `harness serve`
auto-arms (see `maybeAutoArmGoal` below) — the loop actually starts running
once the current turn ends. Plain `harness run` (no `-goal`) has no such
auto-arm step: a tool-driven `set` call registers and journals the goal
(`goal.active` becomes true) but nothing ever calls `PursueGoal` for it, so
it never actually starts evaluating — the process runs its one `Prompt` call
and exits with the goal armed but inert. Only `harness run -goal <condition>`
itself drives `PursueGoal` to completion.

`POST /session/{id}/goal` on a busy session no longer flatly 409s. A running
goal loop updates its condition in place (`status: "updated"`, 200 — no
second loop, no run-slot claim; the loop picks it up at its next turn
boundary). A plain prompt holding the slot with no goal yet active registers
the goal (`RegisterGoal` needs no run slot) and then retries the claim once,
closing the race against that same prompt's own `runPrompt` tail: if the
retry wins the now-freed slot, the loop spawns immediately and the response
reports `status: "started"` (202); otherwise the prompt's tail is still
ahead of us, its own auto-arm check (`maybeAutoArmGoal`) will claim the slot
and spawn the loop itself once that tail finishes, and the response reports
`status: "armed"` (202) — either way the loop starts exactly once, never
zero times, never twice, no further client action needed. This is also how
the `goal` tool's own `set` action takes effect: arming a goal mid-turn, the
same auto-arm path starts the loop the instant the current turn ends. A
workdir held by a genuinely different session still 409s,
unchanged.

No self-clear is deliberate: a goal-supervised agent must never be able to
cancel its own supervision from inside a running turn, so the `goal` tool
has no `clear` action — `DELETE /session/{id}/goal` remains the only clear
path, and it is operator-only.

The goal loop is a **plan-artifact-free, gate-free** control loop: it is
`Prompt` plus a read-only evaluator call, with no plan document, no edit/plan
mode, and no permission gate. It does not violate the no-plan-mode decision
below.

### Session metadata index

`GET /session` and `GET /session/{id}` do not replay a session journal. Each
session log has a sidecar `<id>.index.json` holding one
`engine.SessionIndex`. The index carries every wire `Session` field with a
durable source: timestamps, model, effort, workdir, parent session, task
lineage, message count, usage, durable goal state, queue depth, and
compaction counters.

Before the index, a read of a non-live session called `LoadSession`. That
call decodes every message body and rebuilds the whole history. The handler
then reported a dozen scalars and dropped the rest. The list endpoint paid
that cost once per non-live session (meetneptune/boxes
`docs/design/console-read-path.md`, workstream 1).

The index is a fold of the journal (`engine/index.go`). Three rules keep it
honest.

**It folds records, not memory.** `Session.writeRecord` folds every record
it appends. `ensureLog` folds the header records it writes directly, which
is the one write that does not pass `writeRecord`. Memory is never the
source: `EnqueuePromptDurable` writes its record before it mutates the
queue, so a fold of memory at that instant would disagree with the log it
claims to summarize.

**It is a cache, never an authority.** `ReadSessionIndex` serves a stored
index only when three checks pass: a checksum over the stored bytes, the
journal's byte length, and the journal's modification time. Anything else
refolds from byte 0. There is no repair path, so no repair path can be
wrong. The checksum catches a torn read — both writers replace the sidecar
in place, and a reader in another process can otherwise mix old bytes with
new ones that parse. Length and modification time are a staleness key, not a
proof: they rest on the journal's own contract of one writer and append-only
writes.

**It reports what a full load reports.** `Messages` and `LastActivityAt` run
through `message.ResolveOrphanToolCalls`, over a skeleton of ids, roles, and
tool-call ids. A crash between a tool call and its result therefore counts
the same on both paths. `DurableMessages` counts the records alone, for a
reader that must map a message to a byte offset; paginated message reads are
numbered against it.

Some journals cannot be answered by a fold at all. A legacy header records
no workdir, and a crash can tear away the initial model record. `LoadSession`
answers those from the loading `Config`; a fold has no `Config`.
`SessionIndex.Complete` reports the difference, and
`Server.coldSessionJSON` uses the authoritative load path for such a
session.

Three folds have real state machines. Each has one implementation, shared by
`LoadSession` and the index: `applyCompactRecord` (`compact.go`),
`applyGoalRecord` (`store.go`), and `promptQueueFold` (`queue.go`).
`engine/index_test.go`'s oracle test pins every index field against a full
`LoadSession`.

A refold is far cheaper than a full `LoadSession`, and a current index is
cheaper again. Write-through costs one marshal and one rewrite per record.
The sidecar never gets an `fsync`: losing it in a crash costs one refold.

`server.Options.Plugins` supplies a session's plugins to a cold read.
Plugins are process configuration, not durable session state, and a cold
read has no `Session` to ask.

### Paginated message reads

`GET /session/{id}/message?before_seq=N&limit=K` answers one bounded page of
a session's messages, read from the journal's tail. The unparameterized call
is unchanged, byte for byte: no `before_seq` and no `limit` still returns the
bare array of the whole history every existing caller expects. A request that
names either parameter gets a `MessagePage` envelope instead — `messages`,
`first_seq`, `last_seq`, `total`, `has_more` — because a client that pages
needs the page's position and a client that does not must not have to learn a
new shape. A console loads the tail and pages older messages in on scroll
(meetneptune/boxes `docs/design/console-read-path.md`, workstream 2 and
directive 1). Before this, every console open transferred the whole
transcript.

**Seq is an ordinal over the DURABLE message sequence**: message records in
log order, with each compact record's fold applied (the folded range replaced
by that record's summary). `SessionIndex.DurableMessages` counts that same
sequence, so the newest message's seq equals it. `SessionIndex.Messages` can
be larger — it also counts the synthetic tool results
`message.ResolveOrphanToolCalls` derives — and a derived message has no
record, so it has no seq and no page carries it. This definition is what
makes a bounded read possible: an ordinal over durable records can be counted
backwards from the end of a file, while an ordinal over a materialized
history cannot be known without materializing it.

`engine.ReadMessagePage` (`engine/messagepage.go`) serves a page two ways,
and both are numbered by the same index:

- The **tail walk** reads `revChunkBytes` blocks backwards from
  `SessionIndex.LogSize` and numbers message records down from the total. It
  touches only the tail however long the journal is. It gives up the moment
  it meets a compact record, because the messages a fold KEPT sit in the log
  between the folded range and the compact record itself — undoing that
  backwards would be a second, subtly different implementation of a fold.
- The **fold path** then reuses `indexFold` — the same forward fold, so the
  same `applyCompactRecord` — to learn which ids occupy the requested seqs,
  and reads back just those records. It costs one slim pass (ids and roles,
  never message bodies), which is still three orders of magnitude below
  materializing the history.

The scan is bounded by `SessionIndex.LogSize`, never the file's current size,
so a turn appending records while a page is read cannot renumber that page
under it.

Two properties are deliberate. A page carries durable messages **verbatim**:
it never runs `message.ResolveOrphanToolCalls`. That repair exists to keep a
provider REQUEST valid, this endpoint builds no request, and fabricating a
tool failure in a read view has real production history (see `Server.lookup`'s
doc comment: a healthy child's in-flight tool call rendered as failed in the
console for as long as it kept running). And compaction **renumbers** — a fold
replaces N messages with one summary, so every later seq shifts down by N-1.
A client paging across a compaction can see one page overlap another; message
ids are stable and are the way to de-duplicate.

A page is read from the durable records even when the session is resident, so
one seq definition covers both cases: a resident history can carry messages
the log does not (load-time repairs, recovery's memory-only closers), and
numbering those would give one message two different seqs depending on
residency. A session with no journal at all — created through the API and
never prompted — falls back to its resident history, which for such a session
IS the durable sequence.

### Prompt queue

`POST /session/{id}/prompt_async` against a session already busy (another
prompt, or a running goal loop) no longer 409s — it queues. The prompt is
enqueued durably (`engine.Session.EnqueuePrompt`, persisting a `prompt.queued`
record and assigning a session-monotonic ID) synchronously, before any
response is written — the same enqueue-durable-before-202 shape `RegisterGoal`
already uses for goals, closing the accept-vs-lose race structurally. The
response is 202 either way: `status: "started"` when a turn is now running for
this request's own prompt (an idle claim against an EMPTY queue, or a
freed-slot retry that happens to win and dispatch this same prompt), or
`status: "queued"` (carrying the current depth) when it is durably waiting —
including the idle-claim case where the queue is already non-empty (a
restart refold, or any other drain gap that ever left a prompt stranded):
`handlePrompt` enqueues the incoming text behind whatever is already waiting,
then dispatches the queue's HEAD — not necessarily this request's own text —
into the run slot it just claimed, so a fresh arrival can never cut the
line ahead of prompts already queued. The workdir-held-by-another-session 409
is unchanged — only same-session busy gets queue semantics.

The queue drains FIFO, by queue ID, at every run-slot release, with no
exceptions: `runPrompt`'s, `runGoal`'s, and `handleCompact`'s tails all call
`maybeDispatchQueued`, which claims the freed slot, dequeues the head
(`reason: "delivered"`), and spawns it as a normal prompt turn — whose own
tail repeats the check, so the whole queue drains one turn at a time before
anything else gets a look. `handlePrompt`'s own claim-success path (previous
paragraph) is the one non-tail drain site: an admission-time head-dispatch
for the idle-with-non-empty-queue case, closing the gap a tail-only drain
would otherwise leave open between "session goes idle with a queue still
non-empty" and "the next prompt/goal/compact activity happens to touch it."
This is also where
**queue beats goal auto-arm**: `runPrompt`'s and `handleCompact`'s tails call
`maybeDispatchQueued` *before* `maybeAutoArmGoal` (see above), so a prompt
sitting in the queue when a turn or a compact call ends is dispatched first —
direct user input outranks the background objective — and the goal only
auto-arms once the queue is empty.

**Delivery granularity is per tool-call boundary, not per turn.** Inside
`Session.Prompt`'s agentic loop (`engine/engine.go`), the instant a
tool-result message is appended — after the model made one or more tool
calls and before the next provider request in that SAME turn — the loop
drains the ENTIRE queue, FIFO, in one locked op (`DequeueAllPrompts
("injected")`) and appends the drained batch as a single, durable user
message: the same labeled "OPERATOR MESSAGES" block template
(`operatorMessagesBlock`, `engine/queue.go`, shared by every drain site so a
batch renders identically apart from one parameterized word — this
call site passes `operatorContextTask`, so its header says "continue the
task", never "continue the goal", even when this drain happens to fire
inside a goal loop's worker turn; only goal.go's own turn-boundary drain
below passes `operatorContextGoal`). This only ever
APPENDS — never rewrites an earlier message — so a provider's prompt-cache
prefix stays intact, the same principle the managed-processes ephemeral
status block below relies on, except this message is a REAL, durable
delivery, not a disposable status line. A turn that ends WITHOUT any tool
call never reaches this drain point at all (the model's own end-of-turn
return precedes it), so that path — and anything still queued when it
happens — is left entirely to the mechanisms below. Because `PursueGoal`'s
worker turns run through this exact same `Prompt` loop
(`promptTurnWithRetry`), goal loops inherit tool-call-boundary injection
automatically, with no separate wiring: a prompt queued while a goal's
worker turn is mid-tool-call is delivered inside that SAME worker turn —
matching Claude Code's mid-turn steering granularity — rather than waiting
for the goal's own turn boundary described next.

`PursueGoal` keeps a second, complementary drain at its own turn boundary:
at the top of every turn (the same `snapshotGoal` boundary #77's
condition-update snapshot uses, and before that turn's own tool-call-boundary
drain above has any chance to run) it drains the *entire* queue, FIFO —
catching anything still queued from a turn that made no tool calls at all, or
that arrived in the gap between one turn ending and the next one's snapshot —
and prepends it to that turn's directive as the same labeled "OPERATOR
MESSAGES" block (`operatorMessagesBlock`, `operatorContextGoal` — so its
header says "continue the goal"), ahead of — never replacing — the
ordinary condition/guidance text. The evaluator's condition string is
unchanged by this — it is built from the condition alone, never from the
block or the turn's rendered directive — so goal injection judges only the
goal there; the evaluator's separate transcript field does render the full
history, so it does see the block once the worker turn that received it has
run. Every drained prompt journals its own `prompt.dequeued(injected)` record
before the turn's directive is even built, so it counts as delivered at that
point even if the turn's outcome later turns out stale and gets discarded —
an injected prompt is never re-queued, at either drain site. This means an
abort (`POST /abort`) or a goal clear (`DELETE /session/{id}/goal`) racing a
goal turn boundary consumes an entire just-injected batch at once: every
prompt the boundary drained is already journaled `dequeued(injected)` before
the worker turn even starts, so a turn that gets cancelled or whose outcome is
later discarded as stale still loses all of them together — several operator
messages, not just one — the same exposure class an ordinary in-flight prompt
already has, just multiplied across the whole drained batch. The two drain
sites can never double-deliver the same prompt: `DequeueAllPrompts` is one
atomic, locked pop of the whole queue, so whichever site runs first against a
given prompt is the only one that ever sees it.

Every enqueue/dequeue is a durable record — `prompt.queued` and
`prompt.dequeued`, the latter carrying a `reason` of `"delivered"` (idle
drain), `"injected"` (tool-call-boundary or goal-turn-boundary injection —
both drain sites share the reason, see above), or `"cleared"` (see below) —
journaled and emitted (`EventPromptQueued`/`EventPromptDequeued`) under
`s.mu` in the same critical section, mirroring `RegisterGoal`/`ClearGoal`
exactly. Dequeue always journals *before* the text enters any turn, so a crash
between that journal write and the dispatched turn's completion cannot
double-deliver — the prompt is simply gone from the queue on replay, the same
exposure any in-flight prompt already has today. **Boot never auto-dispatches
a resumed queue**: `LoadSession` folds `prompt.queued`/`prompt.dequeued`
records back into the exact undelivered set, `GET /session`'s `queued` count
reflects it immediately, and it sits there until the next natural drain
trigger (an idle prompt, the next tool-call boundary inside a running turn, or
a goal loop's next turn boundary) — the same settled boot rule goals follow.
`DELETE /session/{id}/queue` is the one explicit clear surface: it journals
`prompt.dequeued(cleared)` for every pending item then 204, idempotent on an
empty queue, and never touches a currently running turn — `POST /abort` is
unrelated and does not touch the queue either way (it only cancels the
in-flight turn's context).

Two v1 limits are deliberate, not gaps: **text-only** (queued prompts carry a
plain string — `QueuedPrompt{ID, Text}` — no attachment machinery, matching
the plain-prompt contract's `parts` being text-only already), and **a
per-request `model` override is silently dropped when the prompt is queued**
— there is no slot in `QueuedPrompt` to carry it through to a future drain, so
a caller that needs a model swap to take effect must re-issue the request once
it is confirmed `started`.

`POST /session/{id}/enqueue` (docs/plans/2026-07-21-durable-enqueue.md) is
`prompt_async`'s durable, idempotent sibling for a caller whose own upstream
ack rides on this call succeeding — an inbox poller or coordinator relay,
not an interactive client. `Session.EnqueuePromptDurable` extends
`EnqueuePrompt` with three properties the plain path deliberately lacks:
write-ahead durability (the `prompt.queued` record is written and, in the
default `session_sync: "fsync"` mode, *fsynced* before any in-memory
mutation or response, so a 2xx is an honest attestation rather than a
best-effort ack — a write/fsync failure returns 500 "enqueue not durable"
instead of the swallowed `lastPersistErr` every other persist path uses), a
caller-issued session-monotonic `seq` deduplicated against a durable
high-water mark (`Session.EnqueueSeq()`, journaled on the record and
rebuilt by `LoadSession` — a seq at or below the mark is a clean 200
`duplicate` no-op, so retries are always safe, including across a process
restart), and torn-write healing (a burned-but-failed queue ID is never
reused, and replay folds same-seq records last-writer-wins). Delivery is the
exact same FIFO/tool-boundary/goal-boundary machinery described above — this
is a new *acceptance* contract, not a new delivery path: durable means
accepted into the queue, and delivery-out is still the queue's normal
at-most-once-per-dequeue machinery, so a crash between dequeue and turn
completion loses that delivery once rather than redelivering it, exactly
like any in-flight prompt (`maybeDispatchQueued`'s "No-double-delivery
equivalence", invariant 7, in server/handlers.go). `GET
/session/{id}/queue` is the paired reconciliation read: the watermark plus
the pending queue (FIFO, `seq` present only on durable-enqueue entries), for
an upstream recovering from its own crash to check what's already inside the
durability domain instead of re-sending blind. `prompt_async` remains the
right choice for an interactive client that has no upstream ack to protect —
it is not going away, and `POST /session/{id}/enqueue` adds no new limits
beyond what queued prompts already have (text-only, no model override).

The `fsync` in "write-ahead durability" above is itself mode-selectable:
config's `session_sync` ("fsync", the default, or "volume") gates both this
durable-enqueue fsync and the one-time session-create directory fsync
(`ensureLog`'s fresh-file `syncDir` call, store.go) — nothing else changes.
"volume" is for a session store on a continuously-synced network volume
whose own commit layer is the documented durability boundary: fsync adds no
durability there, and some FUSE/9p transports deadlock permanently on it
(`fsync(dirfd)` especially — a wedge that hangs every later file op on the
mount, not just the one call). In that mode the write(2) landing out of
`EnqueuePromptDurable`/`ensureLog` is itself the attestation; the write
ordering, torn-write healing, and replay/fold logic above are byte-for-byte
identical in both modes — a volume can still lose an unsynced tail on abrupt
death exactly like a torn fsync can, and the same last-writer-wins fold
repairs both. See docs/deploy-modal.md for the recommended setting on Modal
Volume v2 deployments.

### Managed processes

`config.Config.Processes` (`processes` in JSON) declares named long-lived
dev/support processes (`pnpm dev`, a local DB) that a `process` session
tool can start/stop/restart/inspect without an agent reinventing PID
tracking. `*process.Manager` (package `process`, not `engine`) is a
box-scoped singleton — built once per harness process and shared across
every session, exactly like `engine.MCPManager` — with a
starting/ready/running/exited/stopped state machine, unix process-GROUP
kill on stop (mirroring `engine/bash_unix.go`'s Setpgid/kill-pgroup/
WaitDelay pattern), and asynchronous death detection (a waiter goroutine
flips state to `exited` with no client asking). Logs land at
`<workDir>/.harness/proc/<name>.log`.

The tool can also `declare`/`undeclare` NEW process definitions at
runtime (server-lifetime only, never written to `.harness.json`) — see
`docs/design/managed-processes.md` for the full validation and origin
(`config` vs `runtime`) rules. `harness serve` always builds a
`*process.Manager`, even with zero configured processes, so the tool is
present on every served box; `harness run` keeps the zero-cost-when-
unconfigured rule.

Once at least one declared process has EVER been started (this server
process's lifetime), request assembly appends an ephemeral `[processes:
...]` status block to the newest user message ONLY — as a
`message.EngineContext` part (see "Ambient engine context is a structured,
unforgeable part" above), never persisted into the durable session log,
never touching any earlier message so a provider's prompt cache prefix
stays intact. See `docs/design/managed-processes.md` §4 for the exact
mechanism and why it is safe.

### Model switching

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

### Reasoning effort

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
`EffortOff` — see "Goal loop" above — because it is a classifier the model
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

### Session affinity (prompt-cache routing hint)

`provider.Request.SessionKey` carries a stable, opaque session identifier on
every request the engine builds — one field on the same per-request struct
`Effort` rides, though unlike `Effort` (set at one call site), the engine
sets `SessionKey` to `Session.ID` at all three request-build sites:
`streamTurn` (`engine/engine.go`, the main turn), `runEvaluator`
(`engine/goal.go`, the goal-loop evaluator), and `runCompactionSummary`
(`engine/compact.go`, the compaction summarizer). The field itself is never
persisted; the value it carries (`Session.ID`) already is, as the session's
own identity.

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
time-to-first-token, through the same gateway. Harness sessions re-send the
whole history every request (stateless transcoding), so a long session on
the openaicompat route (a gateway to Fireworks kimi-k3 and similar models)
pays full prefill on nearly every turn without this hint.

### Anthropic cache TTL (default 1 hour)

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

### A second native Responses provider

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

### Lazy MCP tools (deferred schemas)

An MCP server's tools reach the model as full JSON Schemas in the tools
array. A box that wires several large servers therefore pays for hundreds
of schemas on every turn, at the FRONT of the cached prefix. The MCP
CONNECTION was already lazy; the schema cost was not.

`engine/mcp_lazy.go` defers that cost, opt-in. A DEFERRED server's tools
leave the tools array and appear instead as a name-only catalog — one
`name — one-line description` line each — in a system segment placed after
the Agent Skills catalog and before hook (`system.transform`) segments. It
is the same progressive-disclosure staging Agent Skills already use. The
model loads a schema with the `mcp` tool's `select` action, and the loaded
def is back in the tools array on the next request, so a selected tool is
called exactly like a statically registered one. `runAgenticLoop` rebuilds
the request per tool round, so a `select` takes effect inside the same
turn.

Config: `mcp_tool_loading` is `eager` (the default, and today's behaviour
byte for byte), `auto` (defer once the live catalog exceeds
`mcp_tool_loading_threshold`, default 20 tools), or `lazy` (always).
`mcp_servers.<name>.tool_loading` pins one server `eager` or `lazy`;
`auto` is global-only, because the threshold measures whole-catalog
pressure. Any non-positive threshold resolves to the DEFAULT, never to a
floor of 1 — that value would defer every catalog.

Four rules are load-bearing. Do not relax them:

- **A session that does not hold the `mcp` tool defers nothing.** Never
  defer what the session cannot select. A subagent restricted by an agent
  definition that omits `"mcp"` (`restrictTools`) would otherwise lose
  every MCP schema AND the only path to load one back.
- **The `auto` threshold counts the WHOLE catalog**, including a server
  pinned `eager`. A pin says "always keep these loaded", never "ignore
  their cost".
- **The catalog listing sorts by full tool name, in the engine**, not by
  the registry's server-then-tool slice order (the two differ for servers
  `a` and `a0`). The tools array stays byte-stable because the partition
  preserves the registry's order and changes only when a selection does.
- **`streamTurn` resolves the provider BEFORE it computes the tool plan.**
  The plan's `Tools(ctx)` call is what dials a server for the first time
  and spawns a child process for every stdio server. A turn naming an
  unconfigured provider must return before any of that. The plan still
  runs before `mcpStatusSegment`, which is the pre-existing rule that a
  first-attempt failure is reported in its own turn.

The same reorder moved one hook. `chat.params` still runs first and still
fires on every turn. `system.transform` now runs AFTER provider
resolution, so a turn naming an unconfigured provider returns without
firing it — it used to fire, then fail. Building a system prompt for a
request that is never sent buys nothing, and a plugin that counts
`system.transform` calls now counts sent requests. `chat.params` is
unaffected because provider resolution needs the model it returns.

A stale selection is reaped at plan time: a selected name whose server is
CONNECTED and whose catalog lacks it is dropped. That is what keeps an
invented name — accepted while a server was unconnected, where a real name
and an invented one are indistinguishable — out of the effective set. A
selection whose server is still unconnected is KEPT, so it arms itself on
reconnect. The reap is memory-only; replay re-unions the log and prunes
again.

The `mcp` session tool carries two extra actions when the session can
defer, and only then — a session that defers nothing must not advertise an
action with nothing to act on. `search(query)` ranks the live catalog by
keyword: substring matching over lowercased text, scored once per DISTINCT
query token per field (remote name 50, description 10, server name 5, plus
100 once when the whole query equals a name), sorted by score then name.
Tokens split on Unicode letter/digit classes, never the ASCII ranges — an
ASCII split truncates `café` to `caf` and reduces a CJK query to nothing. A
blank query errors rather than dumping the catalog. Both actions are
refused at DISPATCH, not only omitted from the advertised enum, on a
session that can defer nothing. `select(tools)` loads
schemas, and every name lands in exactly one bucket, tested TOP TO BOTTOM:
`already`, `selected`, `pending` (its server is configured but not
connected — it arms on reconnect), `missing` (no connected server holds it,
or the name is malformed). Its `note` is conditional on that outcome: a
`pending`-only batch must not claim its tools are callable next request.
`select` returns NO schemas: the tools array is the one authoritative copy,
and echoing them would write every schema a second time into durable
history.

**Use implies selection.** An MCP tool call that ROUTES records its own
name. Without it, a tool of an eager server — which needs no `select`, and
which the model is told not to select — would lose its schema the moment an
`auto` flip deferred its server mid-task. The gate is per SERVER, not per
session: a server pinned `eager` can never flip, so a record for its tools
could never pay for itself, even in a session that defers a different
server. A plain `eager` config therefore records nothing at all.

**Both writers of the record apply that same gate.** `select` records a
name only when its server could ever defer, exactly as a routed call does.
A record exists only to survive a flip, so "can this server ever flip" has
one answer whichever writer asks. A pinned-`eager` server's tool is still
reported `selected` — it is loaded and callable — and simply records
nothing.

A selection is durable. `mcp.tools_selected` (`recMCPToolsSelected`,
`engine/store.go`) records the names that ENTER the set, and `LoadSession`
unions every record back. It follows `recToolResultRetained`:
engine-internal state, journaled and folded, with no engine event and no
server journal mapping. **Two writers produce it** — `select`, and a routed
MCP call through use-implies-selection. Wiring only the first silently
loses a tool the model used but never selected.

Recovery degrades in one direction. A restored name whose server is absent
or parked is KEPT, so it arms on reconnect. One whose server connects
WITHOUT it is reaped. A malformed name is skipped on replay, exactly as
`select` refuses to record one — one rule at both ends of the record's
life.

Full design, including the durable record: `docs/design/mcp-lazy-tools.md`.

### The tool array is byte-stable across requests

`Session.toolDefs` (`engine/engine.go`) sorts the BUILT-IN tool group by
name. That sort is a prompt-cache requirement, not cosmetics. `Session.tools`
is a map, Go randomizes map iteration on every range, and tools sit at the
FRONT of the cached prefix on every provider — Anthropic caches tools, then
system, then messages. An unsorted build therefore emitted a different tools
array on every request and invalidated the WHOLE prefix each turn, which no
TTL can help.

The defect is invisible to a unit test that checks the tool SET, and it
appears only in live traffic: consecutive turns of one session each report a
full cache write and no cache read, for a byte-identical system prompt. A
new test must therefore assert the byte-stability of the array, not its
membership. The commit that introduced the sort carries the measured
before/after evidence.

Group order stays built-ins, then MCP, then plugins. The other two groups were
already deterministic — `MCPManager.rebuildToolsLocked` sorts by server then
tool, and `plugin.Host.Tools` walks the configured instance slice — so the
sort applies WITHIN the built-in group only. Adding an MCP server must never
reshuffle the built-in block ahead of it. Any new tool source must be
deterministic before it joins this list.

### Deliberately absent — do not add

- **No permission system.** Tool calls are never gated. There is no `permission.ask` hook, no approval UI, no pre-flight rule evaluation.
- **No plan mode.** No edit-mode/plan-mode distinction anywhere in the engine. (The goal loop above is not plan mode — it produces no plan artifact and gates nothing.)
- **No JS runtime and no opencode plugin compatibility shim.** Plugins are native processes.
- **No auth hooks.** Credential injection happens at the network layer (gatekeeper) in deployed environments.

These are settled decisions. Do not propose or implement them.

## Dispatching goal-supervised sessions

- **Completion conditions must demand world-state evidence, never transcript
  claims.** Require branch-verified-on-origin (`git fetch && git status -sb`
  output shown), pasted test output, etc. — not a model's assertion that it
  did the work. Why: an evaluator once declared files created while the disk
  was empty.
- **Push is the durability mechanism.** Commit as soon as the first test file
  exists; push after every green milestone. Why: lease death and loop death
  have each destroyed unpushed work.
- **Write conditions as timeless end-state predicates, never turn-relative
  phrasing.** The condition string is re-sent verbatim in every guidance
  message (`goalGuidance` embeds it in full on each NOT MET re-prompt, not
  just turn 1), so wording like "on the first turn..." or "don't do X yet"
  keeps re-asserting a stale instruction turn after turn instead of describing
  the state the evaluator should actually check for. Why: live-run evidence
  — such phrasing looped 32 turns chasing an instruction that only ever made
  sense once.

## Plugin System

Plugins are separate processes (any language; Go SDK provided) speaking a versioned JSON-RPC protocol over stdio.

- **Manifest cache**: `harness plugin install` runs the binary once and caches its manifest (name, protocol version, hooks subscribed, tool definitions) keyed by binary hash. Startup reads cached manifests only — nothing spawns at boot.
- **Lazy spawn**: a plugin process starts on first hook dispatch or tool call, then stays warm for the session (module-level caches in plugins are expected and fine).
- Sync hooks chain across plugins in config order — each sees the previous plugin's mutations — and every sync dispatch carries a deadline so a hung plugin can't wedge a session.
- **Plugin visibility**: `Host.Plugins()` reports every CONFIGURED plugin — name, spawn state (`not-spawned`/`running`/`errored`/`stopped`), registered tools, subscribed hooks — from the cached manifest plus live spawn state. The `session_info` tool (field `plugins`) and `GET /session/{id}` (field `plugins`) both surface it, so a not-yet-spawned plugin still appears. The engine reads it through the `Hooks.Plugins()` interface method, nil-guarded exactly like the other `s.cfg.Hooks` dispatch sites. The state read is lock-free (`instance.liveState`, `plugin/host.go`): `instance.start` holds `inst.mu` for the whole dial-plus-handshake, and `Host` is a box-scoped singleton shared by every session on the box, so a read gated on `inst.mu` would let one session's plugin spawn stall `GET /session`/`session_info` for every other session too — the same "a hung plugin can't wedge a session" rule above, applied to a status read instead of a hook dispatch. `errored` also covers a plugin that died AFTER a successful spawn (its connection closed, detected via the existing `conn.closed` signal), not only a failed start.

### Hook protocol v1

| Hook | Mode | Purpose |
|---|---|---|
| `event` | async, fire-and-forget | full event stream (batched) |
| `chat.params` | sync, mutating | model, temperature, etc. per request |
| `chat.message` | sync, mutating | messages before they enter the log |
| `system.transform` | sync, additive | append segments to the system prompt (runs after `chat.params`) |
| `shell.env` | sync, mutating | inject env vars into shell/tool commands |
| `tool.execute.before` | sync, mutating/blocking | rewrite args or block with `{deny: "message"}` |
| `tool.execute.after` | sync, mutating | rewrite/annotate tool results |

Plugins may also register **custom tools** (defs in manifest, execution via RPC).

### Plugin client API

Plugins are API clients over the same channel: `Session.Messages`, `MCP.Call`, `Generate` (LLM calls through the harness provider layer — plugins never carry their own API keys), and `plugin.HTTPClient()` (outbound HTTP with harness-configured headers, e.g. workspace attribution).

Events v1: `session.status`, `question.asked`, `file.edited`,
`tool.execute.start`, `tool.execute.end`, `session.error`. Message-delta
events are deliberately deferred (see plugin/PROTOCOL.md) pending a
throttling design.

Capability parity bar: the protocol must be able to express the plugin
patterns common in opencode setups — event-driven activity tracking, token
refresh via `shell.env`, tool-call rewriting/vetoing and result guards via
`tool.execute.*`, path-scoped system prompt injection, and custom tools that
call back into the platform.

## External Protocol Surfaces

Standards we conform to at the edges. The internal model (event log, canonical
messages, hook protocol) is ours; these are adapters, never the internal
representation.

- **ACP (Agent Client Protocol, agentclientprotocol.com)** — the editor ↔ agent
  standard (Zed, JetBrains, Neovim, Emacs). Implemented as a thin adapter in
  `server/` mapping the event log to `session/update` notifications. Where our
  event vocabulary has arbitrary naming choices, prefer ACP's names to keep the
  adapter mechanical. We never send `session/request_permission` (no permission
  system) — an agent that never asks is fully conformant. Note: this is Zed's
  Agent *Client* Protocol, not IBM's dead Agent Communication Protocol.
- **MCP** — client (consume tool servers) and server (expose sessions/tools)
  modes. ACP forwards editor MCP config to us, so the two compose.

  A server's first connect (Initialize+ListAllTools) stays lazy —
  triggered by a session's first `Tools()`/`CallTool()`, bounded by a
  per-server `connect_timeout_s` config field (`MCPServerSpec`, integer
  seconds, <= 0/absent defaults to the engine's own 15s). A server whose
  first attempt fails is never dropped for the process's life: it gets a
  detached background retry on a capped exponential backoff (~1s doubling
  to a 5min cap, jittered) — but bounded to `mcpRetryMaxAttempts` (3)
  further attempts (under ~10s of background effort total). Once those
  are exhausted the entry is marked Parked and the retry goroutine exits
  for good — no further attempt ever fires spontaneously; only an
  explicit re-trigger (the `mcp` tool's `connect` action, below) can move
  it again. A HEALTHY server, by contrast, connects exactly once and is
  never re-probed. `Tools()` always reads live state, so a server that
  recovers mid-session — background retry or explicit reconnect —
  contributes tools on the very next turn automatically, no new session
  required. `CallTool`/`CallServerTool` split the old combined error into
  two: a server name absent from config errors "not configured" (never
  recoverable); a configured-but-unconnected server (still retrying, or
  parked) errors naming that state explicitly (recoverable — retrying may
  still self-heal, parked needs the `mcp` tool). While at least one
  server is degraded, request assembly appends an ambient `[mcp:
  unavailable — <name> (<reason>; retrying), ...]` block to the newest
  user message only — computed fresh every turn, never persisted,
  self-correcting as retries succeed; a Parked server's clause instead
  reads `<name> (<reason>; use the mcp tool action "connect" to retry)` —
  sharing its append-only-to-the-newest-message mechanism
  (`withAmbientStatus`) with the managed-processes status block above.

  A built-in `mcp` session tool is registered in `newSession` whenever
  the session's MCP registry reports at least one configured server (no
  config flag, unlike `GoalTool`). `status` reports every configured
  server's live state — `{name, connected, attempts, parked, reason}`;
  `connect {server}` makes ONE bounded, synchronous attempt for a named
  server — the only path back for a Parked server, though it works
  against a still-retrying or never-yet-attempted one too. An
  already-connected server is a friendly no-op; an unknown name errors
  listing the configured names. A per-server in-flight guard (under the
  manager's own lock) serializes a tool-triggered connect against both a
  concurrent `connect` call and `retryServer`'s own background attempt
  for the same server — whichever gets there first dials, the other
  reports "attempt already in progress." Every model-visible string on
  this surface — the ambient block, `status`'s `reason`, `connect`'s
  failure result — is `classifyMCPConnectError`'s output, never a raw
  error (which can embed the server's endpoint URL and any secret it
  carries).
- **OpenTelemetry GenAI semantic conventions** — for span/metric naming when
  observability lands. Configuration via standard `OTEL_*` env vars only.
- **A2A** — deliberately not implemented. Cross-org agent meshes are a
  different layer; revisit only if a concrete need appears.

## Development hub

`harness hub` is a local, single-operator control surface over a FLEET of
`harness serve` boxes — a fleet dashboard for "what are my agents
doing right now" and for dispatching new goal-supervised sessions, not a
deployed product. It serves one embedded, single-file page
(`tools/hub/index.html`, `go:embed`) on
`localhost:7777` by default (`-addr` to change it).

- **No server-side state.** The hub keeps no registry and reads no config
  file: every box (name, base URL, run token) and the current selection
  live only in that browser tab's URL fragment, base64-encoded JSON
  (`#s=...`), kept in sync via `history.replaceState`. That makes a hub URL
  bookmarkable and shareable between local tabs with zero persistence code
  — and means **run tokens ride the URL by design**; treat a hub link like
  a secret.
- **The page talks to boxes directly** from the browser, over each box's
  normal HTTP+SSE API (`server/openapi.yaml`) — never proxied through the
  hub's own server. Every box must therefore be started with `-cors-origin`
  set to the hub's origin (or `*` for local hacking), e.g. `harness serve
  -cors-origin http://localhost:7777`; a box without it will look
  permanently unreachable from the hub.
- **The Go side is minimal on purpose**, exactly one API: `POST /spawn`.
  It execs the command given by `-spawn-command` (or `$HARNESS_HUB_SPAWN`)
  via `sh -c` and streams its combined stdout+stderr live to the page over
  SSE. The **spawn-command contract** — the only coupling between this repo
  and any deployment-specific provisioning tool — is plain lines anywhere
  in that output: `TUNNEL_URL=<url>` and `RUN_TOKEN=<token>` (required to
  add the box), and any number of `PORT_URL_<port>=<url>` lines (optional —
  one per exposed port's own tunnel/preview URL, collected into a
  `port_urls` map; see the process strip in `tools/hub/index.html`'s header
  comment). Once the command exits, the stream ends with a summary carrying
  those values (if found) and the exit code; the page adds the new box to
  its own URL state itself. Nothing box-provisioning-specific lives in this
  repo.
  - **Box name passthrough.** `POST /spawn`'s JSON body optionally carries
    `{"name": "..."}` — the page's generated (or, on a Respawn/ADOPT, reused)
    box name. The Go handler sets it as `HARNESS_HUB_BOX_NAME` in the spawn
    command's own environment (`tools/hub/spawn.go`'s `runSpawn`), exactly
    the deployment-environment contract `docs/design/fleet-model.md` §8
    specifies: deployment tooling invoked by `-spawn-command` reads this
    variable to derive per-name storage (typically setting
    `HARNESS_SESSION_DIR` from it before `harness serve` starts) — harness's
    own code never reads `HARNESS_HUB_BOX_NAME` at all. A request with no
    body, or no `name` field, spawns exactly as before (no env var set).
- The hub binds loopback-only by default (`resolveAddr` in `tools/hub/hub.go`).
- **Browser-security hardening** (both in `tools/hub/hub.go`, tested in
  `tools/hub/hub_test.go`). `POST /spawn` execs a real, costly provision
  command, so `handleSpawn` rejects a browser cross-origin request before any
  exec: if an `Origin` header is present it must match the request's `Host`
  (OWASP verify-origin). Loopback binding alone does not stop this — any page
  the operator visits can `fetch("http://localhost:7777/spawn",{method:
  "POST"})` as a no-preflight CORS simple request — but the page's own
  same-origin `fetch("/spawn")` (Origin == Host) and non-browser clients (no
  Origin, so not a CSRF vector) pass unchanged. The served page also carries
  a strict `Content-Security-Policy` (`default-src 'none'` + `'unsafe-inline'`
  script/style — the page is a single no-build `go:embed`'d file with no
  external resources and no per-response nonce hook — + `connect-src *`,
  required because it fetches/streams from arbitrary operator-added box
  origins the stateless hub cannot enumerate, + `frame-ancestors`/`base-uri`/
  `form-action` pinned to `'none'`): defense-in-depth for a page holding run
  tokens in its URL fragment.
- **Pure hub logic is unit-tested** by `tools/hub/hub_test.mjs` (run:
  `node --test tools/hub/*_test.mjs`). **End-to-end, against a real backend**
  is `tools/hub/e2e` (see its README): a `go test -race ./...` subtree that
  starts an actual `server.Server` + `hub.NewHandler` and drives the real,
  served `index.html` with Node + jsdom and an unmocked `fetch` — no manual
  setup step; it installs its own `npm` dependency on first run.

### UI design language

The hub is styled as **tactical telemetry** — a committed dark-only
brutalist archetype (derived from the public
[taste-skill](https://github.com/Leonxlnx/taste-skill) brutalist +
anti-slop skills). Any new hub UI — and future passes on the inspector,
which still wears the older soft theme — follows these rules:

- **One substrate, no theme toggle**: `#0a0a0a` background, `#eaeaea`
  phosphor foreground, `#2a2a2a` hairline borders. Never reintroduce a
  light mode here; pick-one-and-commit is the point.
- **Two semantic colors only.** Hazard red (`--accent`, `#ff2a2a`) means
  trouble or destructive action, nothing else. Terminal green (`--ok`,
  `#4af626`) is reserved for exactly one semantic: live or succeeded goal
  execution. Everything else is monochrome.
- **Monospace dominance**: body text is the `ui-monospace` stack;
  headers are heavy uppercase system-ui. Micro-labels are uppercase with
  `.06–.1em` tracking. No webfonts — the page is CSP-self-contained.
- **Geometry**: `border-radius: 0` absolutely everywhere; square status
  markers; 1px compartment borders; inverted-video hover
  (foreground/background swap). No gradients, soft shadows, or
  translucency. The scanline overlay is static — motion requires a
  stated purpose.
- **Copy discipline**: no emoji in UI strings, no em-dashes anywhere, and
  every piece of "telemetry" displayed must be real data (vcs revisions,
  seqs, PIDs, token counts) — never decorative or fabricated metadata.
- **Selectors are load-bearing**: the renderers create elements by class
  name (`.sess`, `.box-card`, `.dot`, `.goalnarr`, …). Restyle classes;
  never rename them in a styling pass.

## Session monitor

`tools/monitor` (`tools/monitor/index.html`) is the single-instance
counterpart to the hub above: where the hub answers "what are my boxes doing"
across a FLEET, the monitor answers "what is THIS `harness serve` instance
doing right now" — a live board of every session on that one box (phase,
current tool, staleness), a per-session detail view with a scrolling
transcript, and a composer to speak into a session (`prompt_async`). Like the
hub and the inspector, it is a build-free, dependency-free single HTML file
with no Go-side handler of its own.

- **How to run it**: open the file directly (`file://`) or serve it from any
  static host — nothing box-specific is baked in. The target box must be
  started with `-cors-origin` covering the monitor's origin (or `*` for local
  hacking), exactly like the hub's requirement above; a box without it looks
  permanently unreachable. The base URL and run token are entered in the
  page itself and persisted to `localStorage` in plaintext (same documented
  tradeoff as the inspector — a dev tool, not for a shared origin with a
  long-lived token) so a reload can reconnect automatically. Routing lives in
  the URL fragment as small explicit params, not the hub's base64 blob:
  `#b=<base>` (box base URL) and `#s=<session id>` (open detail view) — both
  bookmarkable, encoded/decoded by a tested pure helper.
- **Embedded serving, frictionless local**: every `harness serve` box also
  offers its own copy same-origin, at `GET /monitor` — by default
  `http://localhost:4096/monitor` (the port follows `-addr`, default
  `localhost:4096`; the exact URL, with any `#t=<token>` capability suffix, is
  printed to the terminal on startup — `monitorTerminalHint`). The bare root
  is a convenience redirect: `GET /` 302s to `/monitor` (via the `GET /{$}`
  route — `{$}`-anchored to the root path only, never a catch-all — registered
  under the same `MonitorPage` guard, so a pure-API box keeps `/` a clean 404).
  `tools/monitor`
  (package `monitor`, `embed.go`) `//go:embed`s the exact committed
  `index.html`; `cmd/harness`'s `serveCmd` wires it into
  `server.Options.MonitorPage`, which the server serves unauthenticated
  (like `/health` — the page itself carries no secrets) with a
  same-origin-scoped `Content-Security-Policy` (`connect-src 'self'`).
  `server` itself never imports `tools/monitor` (layering: `server` must
  not import `tools/*`); only `cmd/harness` does, the same pattern
  `harness hub` already uses. The `file://`/static-host path is unaffected
  — this is additive, and stays the only option for monitoring a box from a
  different origin (the embedded route's CSP deliberately does not permit
  that).
  - **Unauthenticated-on-loopback** (`server.Options.Unauthenticated`, an
    EXPLICIT opt-in never inferred from an empty `RunToken` on its own —
    `New` still fails closed otherwise): `serveCmd` classifies `-addr`
    (`isLoopbackAddr` — `localhost`, `127.0.0.1`/`::1`, any
    `net.IP.IsLoopback()` address; a bare `:port`, `0.0.0.0`, `::`, or any
    other routable address is NOT loopback). `HARNESS_RUN_TOKEN` unset +
    loopback bind runs the box fully unauthenticated (every route, not just
    `/health`/`/monitor`) and logs a clear warning; unset + non-loopback
    still hard-errors `HARNESS_RUN_TOKEN is required` exactly as before. The
    token guards network reachability, and loopback is a server-verifiable
    proxy for that (unlike, say, `Origin`, which a client controls).
  - **Unauthenticated on a non-loopback bind is also possible, but ONLY via
    a SECOND, separate, EXPLICIT opt-in** — the `-unauthenticated` serve
    flag, or `HARNESS_UNAUTHENTICATED=1` (`envUnauthenticated`, parsed with
    `strconv.ParseBool`; an unset or unparsable value is false, fail-closed
    on a malformed setting). `resolveUnauthenticated` (`cmd/harness/main.go`)
    is the single decision point both serve flags feed: a non-empty token
    always wins (token path unaffected, any bind); an empty token on a
    loopback bind is unchanged from above; an empty token on a non-loopback
    bind needs this opt-in or it still hard-errors exactly as before — the
    opt-in is never inferred from the empty token alone, only from the flag
    or env var. This is for a deployment where a trusted external gate
    already restricts reachability (e.g. a Cloudflare Access-gated tunnel,
    or a sandboxed network boundary) so the token is redundant with that
    gate — `server.Options.Unauthenticated` itself is bind-address-agnostic
    (see its own doc comment); the safety property lives entirely in
    `cmd/harness` deciding WHEN to set it. Landing this on a non-loopback
    bind logs a SEPARATE, distinctly worded loud warning ("serving
    unauthenticated on a non-loopback bind") from the loopback one above, so
    the two are distinguishable in a log search.
  - **Same-origin auto-connect** (`embeddedConnectPlan`, index.html): opening
    a box's own `/monitor` attempts the connection immediately against
    `location.origin`, using whatever token is available (`#t=` fragment,
    then a stored one, then none) — success lands straight on the board, no
    panel, nothing typed (covers both the unauthenticated-loopback case and
    a valid capability URL/returning operator with the SAME call). Only a
    failed attempt (a real token is required and none was available) falls
    back to a minimal token-only panel — host is already known, so the base
    field never reappears. A `#t=<token>` fragment (mirroring `tools/hub`'s
    "run tokens ride the URL by design" precedent, `extractFragmentToken`)
    is adopted into the SAME `localStorage` key manual entry uses and
    immediately scrubbed from the visible URL via `history.replaceState`; a
    fragment `#b=` naming a DIFFERENT origin than this box's own — which the
    embedded route's CSP would block outright if tried — surfaces a
    plain-text notice instead of attempting it, composing with (never
    blocking) the own-origin auto-connect. None of this weakens the auth
    model itself: a token is still required and still checked exactly as a
    hand-typed one would be, on every box that hasn't explicitly opted into
    `Unauthenticated`.
  - On a TTY, `serveCmd` also prints a click-ready line to stderr after
    "serve start" — `monitor: http://<addr>/monitor#t=<token>` (or, when
    running unauthenticated-loopback, the plain URL with no `#t=` at all,
    since there's no credential to carry) — gated on `stderrIsTerminal`
    (`os.ModeCharDevice`, stdlib-only) so a tokenized URL never lands in
    piped/production stderr, only an interactive operator's own terminal.
- **Test layers.** Pure helpers (SSE parser, activity reducer, transcript
  fold, route codec, formatters) live inside index.html's `/* TESTABLE-BEGIN
  */ … /* TESTABLE-END */` region and are covered by
  `tools/monitor/monitor_test.mjs` (run: `node --test tools/monitor/*_test.mjs`),
  using the same extraction-and-`vm`-evaluate pattern as the inspector's
  `inspector_test.mjs` (and now the hub's, above) — no build step, so the
  tests read the region straight out of the committed HTML. End-to-end,
  against a real backend, is `tools/monitor/e2e` (see its README): a `go
  test` subtree that starts a real `server.Server` plus a plain static file
  server for the actual committed `index.html`, and drives it with Node +
  jsdom and an unmocked `fetch` — mirroring `tools/hub/e2e`'s structure and
  conventions. A `window.__monitorTuning = {QUIET_MS, STALL_MS}` seam (set
  via jsdom's `beforeParse` before the page's own script runs, a no-op in
  production since nothing else ever sets it) lets both the unit and e2e
  suites shrink the staleness thresholds so `quiet`/`stalled` transitions are
  observable in test time instead of real minutes.
- **UI design language.** The monitor deliberately does NOT inherit the
  hub's committed dark brutalist archetype above. It carries its own
  "instrument sheet" language: light-first with a dark variant, both driven
  by one OKLCH token set (`--surface`, `--text-1..3`, `--separator`, etc.);
  semantic green/amber/red (`--ok`/`--warn`/`--critical`) are reserved
  strictly for session/staleness state, never decoration; a single accent
  color is owned by interaction (the composer's send affordance is the
  page's only filled-accent control). `docs/design/monitor-mockup.html` is
  the user-approved visual spec — its tokens, grid template, and markup
  shapes are binding; restyle within that spec rather than importing the
  hub's theme onto it.

## Fleet model (the deploy story)

The full build spec lives in `docs/design/fleet-model.md` — read it before
touching anything box-identity, session-lineage, or goal-pause related. The
short version this repo's code assumes: identity is an operator-chosen box
**NAME**; storage is one volume/directory per name (`HARNESS_SESSION_DIR`
points at it), never shared between concurrently-live servers; a box is
ephemeral compute serving one name (cattle), the name and its volume are
durable (pets). Respawning the same name over the same volume is **ADOPT**
— history restores, and any goal that was armed when the box died surfaces
as `paused`/`pause_reason: "restart"` (see the goal loop's paused
presentation, `engine/goal.go` and `server/journal.go`'s `goal.paused`
record) rather than a false "still running" reading. `parent_session`
(`POST /session`, see `engine/store.go`) is the lineage thread connecting a
re-dispatch to the task it continues from, so a fleet UI can group a box's
history by task across boxes.

Subagent lineage is durable. `SessionManager.Spawn` records
`task_parent_id`, `task_agent_type`, and `task_depth` on the child's
session header (`engine/store.go`), and appends each child id to the
parent's own log. `LoadSession` restores all of them with no
SessionManager adoption needed. `GET /session/{id}.lineage` prefers the
durable `task_depth` over the live tree's derived depth, and merges live
children with the durable spawn list (`childIDsUnion`,
`server/handlers.go`) — so `lineage.depth` and `lineage.children` survive
`Reap` and a process restart. `childIDsUnion` merges both sides through
ONE de-duplicating loop and trusts neither side to be duplicate-free: an
id appears exactly once, whichever side carried it. Never re-add a
per-side fast path that skips the merge — an earlier one copied `live`
verbatim when `durable` was empty, so one repeated id survived or
collapsed depending only on whether the OTHER argument had anything in
it. A legacy header without `task_depth`
restores 0; `adoptReloadedLocked` then falls back to the `m.maxDepth`
refusal sentinel, exactly as before the field existed.

A failed child's `fail_reason` carries the CAUSE, not only a class.
`classifySpawnFailure` (`engine/session_manager.go`) builds it as a fixed
classified prefix, then the underlying error message — masked with
`maskSecrets` and capped at `spawnErrorDetailCap` (500) runes. One prefix
covers a whole family of causes (a permanent 400 is a malformed request
AND a quota rejection AND a policy refusal), so a parent that reads only
the prefix must guess: a live incident measured that guess as "respawn a
sibling straight into the same fleet-wide provider wall". The #82 leak
rule still holds in its narrower form — never surface a provider error
RAW — through masking plus the cap, the same best-effort trade a retained
tool result already makes. `context.Canceled`/`context.DeadlineExceeded`
keep their short fixed `canceled`/`timed out` strings, with no cause
appended. The reason reaches the parent through the `[tasks: ...]`
notification, `SessionNode.FailReason` (so `task status` and
`GET /session/{id}.lineage.fail_reason`), and the journal's
`task_fail_reason`.

Server-side session resolution has ONE entry point: `Server.resolveLive`
returns a `liveSession` snapshot (`server/live.go`) that holds the
residency half (`Server.sessions`, one `s.mu` hold) and the SessionManager
half (one `SessionAndInfo` hold) together. Read a session, its status, or
its lineage from that snapshot — never from `s.sessions` or `sessMgr`
directly, and never take a second manager read later in the same request.
The two halves are separate holds on purpose: `server.mu` is a leaf lock
with respect to `SessionManager.mu`, so one atomic hold over both would
build the cycle that rule forbids. Residency wins whenever it has an
answer, because a resident session's own `running` flag is authoritative
for itself (`freeRunSlotAndEmitIdle` clears it before `ReportTurnEnd`
flips the node). The manager half answers only what residency cannot: a
Spawn-driven child, which is never a residency key.

**Provider exhaustion is not a child failure.** An ACCOUNT-level supply
wall — the API key's usage limit, quota, credit balance, or spend cap — is
FLEET-WIDE (every sibling on the same key hits the identical wall at the
identical moment) and TEMPORAL (the child's session and work are intact and
re-runnable once the provider's clock rolls over). A parent that reads it as
an ordinary failure respawns a replacement into the same wall, which a live
incident measured. Three layers carry it:

- The ADAPTER classifies, never the engine. `provider/anthropic`'s
  `parseUsageExhaustion` gates on the HTTP status (400/402/403/429, or none
  at all for a mid-stream `error` event) and then matches
  `usageExhaustionPatterns` — a deliberately flat, extensible list of
  observed wall wordings, one regexp per shape, each of which must name a
  spent SUPPLY, never a per-minute THROTTLE. It returns a
  `provider.Error{Kind: ErrKindProviderExhausted, RecoverHint}` wrapped
  permanent (no backoff outlives a spent quota). This is the second place
  message matching is tolerated, under `parseContextOverflow`'s rules. Other
  adapters opt in by producing the same kind; only anthropic does today.
- The ENGINE reads the typed classification, never text.
  `classifySpawnFailure` (`engine/session_manager.go`) maps
  `provider.AsProviderExhausted` — or a `RetryableRateLimited` class that
  outlived the retry budget — to `FailKindProviderExhausted`
  (`"provider_exhausted"`). Overload and 5xx weather deliberately do NOT
  qualify: those clear in seconds and a sibling may well succeed.
- The STATUS VOCABULARY is unchanged. An exhausted child is `StatusFailed`,
  with the kind in a SEPARATE `FailKind` field (`SessionNode`,
  `taskNotification`, the durable `taskNotifyRecord`, `task status`'s
  `fail_kind`, `GET /session/{id}.lineage.fail_kind`, the journal's
  `task_fail_kind`). A sixth `SessionStatus` value would have forced every
  cancellation/Reap/delivery/restore switch to grow an arm that behaves
  exactly like `StatusFailed`; only the PARENT's next move differs.

The rate-limit arm conflates a spent quota with a per-minute throttle that
outlived the child's small `PromptRetries` budget, one-directionally and on
purpose: a missed wall makes a parent respawn into it (the incident), while
a false wall costs one deferred resume of an intact child, and a hintless
guidance names no waiting period. An adapter that classifies its own quota
shape never reaches that arm. Both the cause and the recover-at hint go through
`boundedProviderText` (mask, then cap), so model-visible provider text on
this surface has one rule, not one per field. The hint is stated in ONE
engine-authored place — `taskFailureGuidance`'s "after <hint>" — never in
the reason prefix as well: the hint is extracted FROM the provider message
the reason already quotes, so naming it there made one rendered line
repeat the same time three times. It rides the durable record
(`taskNotifyRecord.FailHint`) because that guidance clause is now the only
carrier of the fact.

`taskFailureGuidance` (`engine/taskdelivery.go`) appends the parent's
instructions to that child's own notification line — child preserved, do not
spawn a replacement, resume with `task send` on this session id, after the
recover-at hint when the provider gave one. Resuming is the existing
send-to-a-settled-descendant re-run path, unchanged. A turn that then
succeeds clears `failReason`/`failKind` on the node, so a resumed child
stops reporting a wall it already got past; `finalizeTurn`'s
`alreadyCanceled` branch clears them too, since a CANCELED re-run must not
keep snapshotting a classification no live cancellation sets and
`restoreKnownStatusLocked` restores as empty.

A REAPED descendant is still resolvable, not "no such session." `Reap`
collects a done/failed/canceled LEAF the instant it settles (its own doc
comment), which a caller that spawned it has no way to observe before
asking about it again — a live incident hit exactly this: `task send` to
a settled child answered `no such session` depending on internal reap
timing the parent could not see. `resolveOrReviveDescendantLocked`
(`engine/session_manager.go`), the shared first step of all four verbs,
falls back to disk when a live-tree lookup misses: `LoadSession` the
target, then confirm ancestry from its own DURABLE `task_parent_id`
chain (`durableAncestorChainHas`) — never from live state alone, which is
exactly what `Reap` already erased. Only a target with no session log on
disk either, or whose durable lineage does not reach the caller, still
answers `ErrUnknownSession`/`ErrNotDescendant`. The four verbs then
diverge on what a successful disk resolution does: `send` RE-ADOPTS the
revived child into the tree (`adoptReloadedLocked`, the same
adopt-on-first-sight path `AdoptReloaded`/`handleSpawnChild`'s
parent-lookup fallback already use) and re-runs it exactly like a
settled-but-unreaped child — `budgetedByChild` surviving `Reap` by design
is what stops that re-adopt from double-crediting its already-spent
usage. `status`/`log` serve the disk-loaded state directly
(`durableSnapshot`/`deriveSettledStatus`) WITHOUT re-adopting: a
read-only, poll-shaped verb must not have the side effect of pinning a
reaped descendant back into memory. `cancel` on a reaped target is a
no-op success (nothing left to interrupt) reporting its real terminal
status, never `StatusCanceled`. The disk-bound half of this resolution
runs with `m.mu` released — one slow disk read must never stall every
other session's `Info`/`Reap`/`Spawn`/`Send` call — and re-validates
`m.nodes[targetID]` on reacquiring the lock, so a concurrent adopt of the
same id (another `Spawn`, `AdoptReloaded`, or a second racing revival)
always wins single-handedly: whichever adoption reaches `m.nodes` first
is authoritative, and the loser's own "already managed" is ignored, the
same rule `AdoptReloaded`'s existing callers already follow for that
race.

A parent can read a dead child's tail. The `task` tool's `log` verb
(`runTaskLog`, `engine/task_tool.go`, over
`SessionManager.DescendantTranscript`) returns the last N transcript
entries of a descendant, LIVING OR DEAD, under the same ancestor gate
(`isDescendantLocked`, or the disk-backed lineage check above once
`Reap` has removed the live node) cancel/status/send use — a terminal
node keeps its `*Session`, history included, until `Reap`, so no reload
and no disk read is involved for a still-tracked descendant. It is
bounded on three axes, because its output lands in the
PARENT's context and replays on every later turn: `tail` (default 20,
clamped at 100, a negative value is an error), a per-entry rune cap, and a
total rune budget filled NEWEST-first so the messages nearest a death
always survive. The reply reports the descendant's whole message count
next to how many entries came back, so a model knows it is reading a
window, and it carries `fail_kind` alongside `fail_reason` — the same
structured half `task status` reports, so a reader with the tail in front
of it never needs a second call to learn a death was an account wall
rather than the child. Every non-text part is rendered rather than dropped — a tool call
with capped arguments, a tool result, a reasoning summary, and an
attachment COUNT that includes blobs nested inside a tool result, which
`Parts.Text()` itself drops. Content is deliberately NOT masked: parent
and child are the same operator's sessions in one process, and a child's
final text already reaches the parent verbatim in its completion
notification.

`Config.OnRequest` receives the firing session's own id as its first
parameter (`engine/engine.go`). Never wire it as a closure over a
captured session variable: `configSnapshot` copies the func value into
every spawned child, which misattributes the child's `request.meta`
records to the closed-over session's id.

**Hub spawn contract:** the hub that spawns boxes — `harness hub`, now
implemented in `tools/hub/` (see the Development hub above) — passes the
generated box NAME to the spawn command's environment as
`HARNESS_HUB_BOX_NAME`, so deployment scripts can derive per-name storage
(e.g. mount/create a volume named after it) without the hub and the box
needing any other side channel to agree on identity. Harness itself never
reads this variable — it is a contract between the hub and deployment
tooling, documented in `docs/design/fleet-model.md` §8.

## Serve-mode latency diagnostics

A caller that waits seconds for a reply cannot tell, from outside the
process, whether `harness serve` was slow, the network in front of it was
slow, or the whole process was stopped by garbage collection. Three
threshold-gated WARN lines answer that, and nothing runs always-on.

- **`slow request`** (`server/timing.go`). `serveTimed` wraps the mux
  dispatch in `Server.ServeHTTP` and warns when this process took longer
  than `slowRequestThreshold` (500ms) to answer, with `method`, `route`,
  `status`, `duration_ms`, and the caller's `X-Request-Id` as
  `request_id`. The route is `http.Request.Pattern`, which the mux sets
  during the dispatch, so a session id never reaches a log line; a request
  that matched no route logs the fixed `unmatched` label, because the path
  is caller-controlled. `requestID` drops a header value over 64 bytes or
  carrying anything outside one printable ASCII token — it is untrusted
  input that lands in a log line. `longLivedRoutes` exempts `GET /event`
  and `GET /session/{id}/wait`: both run for as long as their caller
  wants, so timing them would warn for every healthy client. Keep that map
  in step with any new streaming or long-poll route.
- **`long gc pause`** (`cmd/harness/gcwatch.go`). A stop-the-world pause
  stops every goroutine, so the process logs nothing at all while it lasts
  and looks exactly like a wedged handler. `gcWatcher` samples the
  runtime's `/gc/pauses:seconds` histogram every 5 seconds and warns about
  a new pause at or past 200ms. It reads `runtime/metrics`, never
  `runtime.ReadMemStats` — `ReadMemStats` itself stops the world, so
  sampling it would add the pause this watcher exists to find.
  `longest_pause_ms` is the LOWER bound of the highest bucket that gained
  a pause, since a histogram records a range. The first sample reports
  nothing: the counts are cumulative for the whole process life. Same
  lifecycle as `inFlightWatchdog` — one cancelable context, cancelled when
  `serveCmd` returns.
- **`/debug/pprof/`** (`server/pprof.go`, `Options.PProf`, `harness serve
  -pprof`). OFF by default, and authed like every other route when on. It
  is the third step, not the first: `GET /debug/goroutines` needs no flag
  and already answers "what is this process blocked on". Turn `-pprof` on
  for a process under investigation when the CPU, heap, block, or mutex
  profile is what is missing. Importing `net/http/pprof` also registers
  these handlers on `http.DefaultServeMux`; this binary never serves that
  mux, so the authed routes here stay the only reachable surface.

No metrics, no tracing, no always-on profiling. A new diagnostic in this
area is a threshold-gated log line or it does not land.

## Startup Speed Rules

- Nothing touches network, subprocesses, or disk beyond one config file before first paint. Provider auth validates on first message send, not at boot.
- No `init()` side effects. No reflection-heavy config frameworks. One flat config parse.
- Pure Go only — no cgo (use modernc SQLite if SQLite is needed) so cross-compilation stays trivial.
- Batch TUI stream rendering (~30–60fps coalescing); never repaint per token delta.

## Development Commands

```bash
go build ./...
go test -race ./...
go test -race -run TestName ./engine/
go vet ./...
```

## Testing

**TDD is mandatory.** Write the failing test first, watch it fail, then
implement until it passes. New behavior lands in the same commit as its test;
a bug fix starts with a test that reproduces the bug.

Rules:

- **Timer-dependent and concurrency-timeout logic is tested inside a
  `testing/synctest` bubble** (Go 1.25+): time is fake and advances only when
  every goroutine in the bubble is durably blocked, so timeouts fire
  deterministically and instantly. `net.Pipe` and channel-based plumbing work
  in bubbles; real network and file I/O do not. Note fake time stops
  advancing once the test function returns — a goroutine parked in
  `time.Sleep` at bubble end is reported as a deadlock, which is the bubble's
  goroutine-leak detection working for you.
- **For concurrency-sensitive code (locks, queues, backpressure), write the
  invariants down in the brief/design before implementation** and test
  against them. Deriving the design from review findings one round at a time
  took four rounds on a recent PR.
- **Exception — cross-process observation** (`e2e/`, and the packages whose
  own subprocess machinery is under test: `process/`, `engine/bash_pipe_test.go`,
  the `live`-tagged tests that call a real remote model): a test may observe
  out-of-process state with deadline-bounded poll loops, because no
  in-process channel crosses an OS process boundary. Every such wait goes
  through `internal/testpoll` — never an inline sleep loop. Its timeout is a
  FAILURE bound, never a synchronization delay: the happy path returns on
  the first successful check. Anything observable in-process still uses
  channels or synctest.
- **In-process state gets a seam, never a poll loop.** A wait on a manager's
  status, a server's session state, or a queue depth blocks on a signal.
  Three production seams exist for exactly this, and a new wait extends one
  rather than sampling: `engine.SessionManager.Changed` (a node's status,
  finalized flag, or tree membership settled — arm it BEFORE the read, so a
  transition landing between read and wait is still delivered),
  `process.Manager.WaitExit` (blocks until a managed OS process has exited,
  and returns that instance's own terminal state — never a later
  restart's), and `GET /session/{id}/wait?until=idle` (the production
  long-poll, which also spans a queue drain). Sampling one of these on an
  interval is a guessed deadline: it flakes under load, and it turns a real
  hang into a slow pass.
- **`time.Sleep` is banned in test code. Absolute — not "for
  synchronization," not "just 10ms," not behind a helper. There are exactly
  two sanctioned time mechanisms in tests: a `testing/synctest` bubble, or
  an injected fake clock/timer seam.** If code under test reads real time
  and cannot run in a bubble, the fix is to add the seam to the production
  code, not to sleep in the test. Reviewers treat any `time.Sleep` in a
  test diff as an automatic blocker; do not push one expecting discussion.
  The single carve-out is cross-process observation (the exception bullet
  above), and only through `internal/testpoll` — never a bare sleep loop
  written inline. To simulate a hung component, block on a channel closed
  in `t.Cleanup`; in a bubble the hang deterministically outlasts any
  timeout with zero wall-clock cost, and the cleanup release lets the
  goroutine exit before bubble end.
- **No guessed deadlines.** Block directly on channels for expected events
  and let the test binary timeout catch hangs; don't wrap waits in short
  arbitrary `time.After` failsafes that flake under load. The rule binds the
  Node/jsdom end-to-end scripts too (`tools/hub/e2e/real_e2e.mjs`,
  `tools/monitor/e2e/real_e2e.mjs`): each waits on a CONDITION through its
  own `waitFor` helper, never `await sleep(N)` followed by an assertion. A
  fixed sleep before an assertion fails two ways at once — it flakes when
  the real round trip runs long, and it passes VACUOUSLY when the state it
  checks is trivially still the pre-action value. A wait for the state a
  negative assertion needs (the render that must not move the viewport)
  makes the assertion mean what its message claims.
- Always run with `-race`; CI runs `go test -race ./...`.
- `t.Helper()` in every test helper; `t.Cleanup` over `defer` in helpers so
  cleanup composes.
- `httptest` for HTTP surfaces; in-process pipes (`net.Pipe`) for protocol
  tests — never spawn real subprocess fixtures unless the subprocess
  machinery itself is under test.
- Table tests where cases multiply; golden JSON comparisons for transcoders
  (struct field order makes marshaled output deterministic).
- Production timers use `time.NewTimer` + `defer Stop()`, not `time.After`,
  when the surrounding function can return before the timer fires.
- **Regression tests must be red-verified.** Prove the test fails against the
  pre-fix code — revert the fix, observe red, re-apply it — and show that
  evidence. A regression guard that never ran red is unverified.
- **Red-verify the NAMED mechanism, not just some failure.** A test name is a
  claim. Revert the exact mechanism the name asserts, then confirm THAT test
  fails for THAT reason. A test that passes from birth, or that goes red for
  an unrelated reason, is not a guard. (Incident: three tests on one branch
  were green against the exact defect they were named for.)
- **Verification drives the production entry point.** Call the same function
  production calls. A check that builds, normalizes, or repairs its input by
  hand proves nothing about the path a user takes — it verifies the
  preparation. (Incident: a fix was reported "verified end-to-end" from a
  test that called `Normalize` by hand. That skipped the `LoadSession` resume
  path, which was the only path that mattered.)
- **An oracle never imports the implementation.** Derive a property-test
  oracle from the external contract — the provider's wire rules, the API
  spec. A predicate that calls a production symbol, or copies its logic,
  cannot fail on a wrong definition, which is the defect class an oracle
  exists to catch. (Incident: `hasOrphanToolCall` was rewritten to call the
  production `hasToolCall`, and then could not see the data loss beside it.)
- **Assert the surplus direction too.** A count check that only looks for
  what is missing passes a payload that ships two of something where one
  belongs.

## Working model — director and coordinator

The director sets direction; the agent runs as tech-lead coordinator. The
director wants speed and ownership: run the pipeline, and surface only what
genuinely needs a human.

- **The pipeline.** Decompose work into tasks. Dispatch one fresh
  implementation agent (fast mid-tier model) per task in an isolated git
  worktree; a strongest-tier reviewer then drives the PR to ZERO findings;
  then merge. Parallelize tasks with disjoint files; sequence tasks that
  share files, to avoid self-inflicted merge conflicts. See "Subagent model
  strategy" for the tier split and "One agent per plan task" below.
- **Status cadence.** Report at MILESTONES and DECISION POINTS, not per
  event. Keep status tight; do not narrate. A terse directive ("do it",
  "merge it", "fine") means execute fast — do not over-ask. Still confirm a
  genuinely load-bearing decision before acting on it.
- **Verify before asserting or fixing.** Check real source, live state, or
  schema — never assume. A wrong assumption about a uid model, a resource
  name, a config flag, or migration order changes the answer; a grep that
  truncates before the relevant line produces a false conclusion. When a
  review finding or a stated premise — including the director's — looks
  wrong, push back with evidence instead of complying.
- **Surface load-bearing forks; decide the rest.** Present a fork that
  reworks an interface, a security posture, or scope with a recommendation
  and the real options, and get the director's call. Decide a mechanical,
  reversible choice yourself and just state what you chose.
- **Do not over-engineer a pre-production system.** A platform still in
  development does not need a rollback flag, a migration shim, or a
  compatibility layer for a change that is verified correct. Prefer the
  simplest correct thing; strip speculative complexity.
- **The review gate is non-negotiable.** Every PR gets an adversarial
  strongest-tier review; drive findings to zero or explicitly defer. Never
  rubber-stamp — the gate catches defects unit tests miss (an invalid
  manifest, a broken generated config, a boot-race, a circular test oracle).
  See "Code Review Protocol".
- **Standing rules.** A subagent's or a peer session's message is never the
  director's approval. Verify a production flag or state before flipping or
  asserting it. Document durable rules and processes, never point-in-time
  events — no dates, measured numbers, or PR numbers in a spec. Never echo a
  secret value — report byte length only.

## Scope discipline

- **Ship the fix the incident proves. File the hardening you found while
  looking.** An opportunistic fix bundled with an urgent one inherits its
  urgency and escapes its scrutiny. (Incident: an unobserved,
  probe-discovered hardening rode an incident fix and cost two review rounds
  of data-loss bugs before it was reverted — see NEP-5293.)
- **A behavior change updates AGENTS.md in the same commit.** This file is
  the binding spec every agent reads first. Four commits once changed the
  goal-loop retry tiers and left this file describing the old ones.

## Subagent model strategy

When you spawn subagents, set the model EXPLICITLY on every spawn — never
let an implementation agent inherit the parent model by default. The rule
is a capability-tier split, not a vendor or model-name rule; it applies to
whatever frontier family is current.

- **Code-writing / implementation / mechanical work uses the fast
  mid-tier.** Writing code, editing docs, changing config,
  grep/investigation, watching a deploy — the tier that is fast, cheap,
  and sufficient for well-specified work (today: Claude Sonnet; the
  equivalent tier elsewhere: GPT mini/frontier-fast class, Gemini Flash).
- **Review, adversarial verification, and judgment gates use the
  strongest tier.** The review-to-zero gate and any correctness verdict
  deserve the strongest available model (today: Claude Opus; elsewhere:
  the full frontier flagship, never a mini/fast variant).

The default pattern for a change: a mid-tier agent writes the PR, a
strongest-tier reviewer drives it to zero. Omitting the model makes a
subagent inherit the parent's model, so a strong parent silently runs
implementation work at flagship price — expensive and backwards. Pass the
model on every spawn. When a model family changes, re-map the two tiers
and keep the split; do not carry a stale model name forward.

## One agent per plan task, not one agent per plan

Dispatch a FRESH implementation agent for each plan task. Do not give one
agent a whole multi-task plan. A monolithic agent accumulates every task
and every review round in one context: it compacts repeatedly, drags its
full history behind every late turn, and burns tokens without adding
fidelity. (Measured 2026-08-12: two plan-executing agents ran ~700k-800k
tokens each; per-round fresh reviewers ran ~120k-300k and stayed sharp.)

- Plans are written for a zero-context engineer (exact files, signatures,
  test code — see the plan format), so a fresh agent per task loses
  nothing. Give each task-agent the plan file path and its ONE task.
- Reviewers stay fresh per round, as they already are.
- Keep one agent across tasks only when the tasks share heavy state that
  a plan file cannot carry (a live debugging session, an unreproducible
  environment).
- Give every dispatch exact file:line pointers instead of letting the
  agent re-derive repo context by grep.

## Never end a turn to wait on an external event

An agent that ends its turn "waiting" on an external event can wait
forever. An external event is any completion signal from outside the
agent's own tool calls: a GitHub workflow run, a deploy, a remote queue.
No notification arrives for an external event. Six agents stalled this
way on 2026-08-12 alone.

- Watch a workflow run with `gh run watch <id> --exit-status` in the
  foreground. Do not poll once and yield.
- A spawned subagent (a reviewer) DOES notify on completion — but its
  reply can misroute when it cannot resolve your address. If a verdict
  is overdue, message the reviewer and ask; do not keep waiting.
- To wait on any other external event, use a blocking command in the
  foreground, or the Monitor tool with an until-loop when the harness
  blocks foreground sleep. End your turn only when the task is complete
  or you are blocked on input only a human can provide.

## Debugging invariants

Rules learned from production incidents (2026-07-09), written so they apply
without knowing the incidents:

- **Cleansing marshals hide poison.** Persisted session logs are scrubbed by
  the guarded marshal paths (`ToolCall.safeArguments` normalizes,
  `ProviderData.MarshalJSON` drops empty entries), so on-disk state can be
  provably clean while resident in-memory state is unmarshalable. When a
  resident session misbehaves but its journal round-trips cleanly through
  `engine.LoadSession` + `json.Marshal`, the defect lives in memory between
  ingest and persist — do not conclude from a clean log that no defect
  exists. (Incident: truncated `ToolCall.Arguments`, fixed in the commit
  titled "fix(message,engine): truncated ToolCall.Arguments must never
  poison history"; see also the tests in `engine/tool_call_poison_test.go`.)
- **Error text names the rejection, not the cause.** Treat error strings as
  the symptom surface — enumerate which layer actually produced the
  credential/config/input being rejected before acting. (Incident: a git 403
  citing SAML SSO was actually a system-level gitconfig credential helper
  serving a rotated-stale token; the SSO re-auth it demanded was
  irrelevant.)
- **Verify binary identity before blaming staleness.** A deployed binary's
  exact commit is embedded — `go version -m <binary>` shows
  `vcs.revision`/`vcs.time` — check that before hypothesizing that a fix is
  missing from a running process.

## Commit messages and PR descriptions

Model: https://go.dev/wiki/CommitMessage. A commit message is documentation
for a future reader who has none of your context; the diff shows what
changed, the message must carry everything the diff cannot.

Subject line: [Conventional Commits](https://www.conventionalcommits.org/),
`type(scope): description` (e.g. `fix(server): stop false-idle wake
between back-to-back turns`) — the repo's existing convention. Lowercase
description, no trailing period, under ~72 chars; scope names the primary
package.

Body — required for every non-trivial change, written as prose, wrapped
~76 columns, in this shape:

1. **The problem, as a story a reader can follow.** What was observably
   wrong, who hits it, how it was found. Not "fix race in waitSnapshot" —
   say what the caller experienced ("a waiter on until=idle could wake in
   the gap between a turn marking idle and its tail dispatching the next
   queued prompt, and read a transcript that was about to change").
2. **Why this design.** The mechanism chosen and the reasoning — including
   alternatives considered and rejected, and why. If review or a design
   fork shaped the outcome, say so ("a suppression design was abandoned
   because collectUntilIdle depends on unconditional idle emission").
3. **The semantic change.** What is now true that was not, stated
   precisely, including deliberate non-changes ("status reporting is
   unchanged; only the waiter's wake condition tightened").
4. **Verification when it earns trust:** red-verified counts, live-fire
   evidence, hammer runs.

PR descriptions follow the same shape at PR granularity: a reviewer must
be able to understand the problem, the approach, and what to scrutinize
before opening a single file. A one-line PR body on a multi-file change
is a defect.

`Fixes #N` / `Updates #N` trailers when an issue exists. Never include
AI-attribution lines (`Co-Authored-By` for agents, session links,
"Generated with" footers) in commits or PR bodies. Squash merges inherit the PR
title as subject — write PR titles to the same standard as commit
subjects.

## Writing Style

You MUST use ASD-STE100 Simplified Technical English — the aerospace
controlled-writing standard — for all prose you write in this repository
(doc comments, docs/, commit messages, PR bodies, reports):

- One word for one idea — pick a term and reuse it verbatim; a synonym
  reads as a second concept.
- Short sentences: ≤20 words for instructions, ≤25 for descriptions.
- Active voice with an explicit subject ("Run `pnpm check-types`", not
  "type checks should be run").
- One topic per paragraph; simple common words ("use" not "utilize").
- Name the thing — never "the code", "the system", "this"; name the
  function, file, or table, with file:line when you have one.
- No hedges or filler ("it's worth noting that", "in order to").
- Exception: error messages, code, identifiers, and paths are quoted
  verbatim, never simplified.

## Code Style

- Standard Go conventions, `go fmt`, `go vet` clean.
- Type annotations in exported APIs over cleverness; small interfaces.

## Code Review Protocol

PRs merge only after the latest automated review round has been read **in
full — including the summary comment**. Inline-thread count is not a merge
gate: the reviewer files findings both as inline threads and as items in the
top-level summary, and both must be addressed (or explicitly acknowledged as
deferred) before merge. Iterate until a round produces zero findings.

A green check is not a review. The reviewer has failed silently before: an
instant API error produces a placeholder comment and zero findings, which
reads as mergeable. Before merging, verify the review summary is substantive.

Read and act on every review thread individually — never batch-resolve. One
explicit resolve command per thread id. A batch operation once resolved
unread findings.

