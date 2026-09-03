# Engine request cycle and tool behavior

This document is the technical system of record for the engine request cycle
and tool behavior. Read only the sections relevant to the change.

## Core invariants

- **A session is an append-only log of typed events.** User messages, model deltas, tool calls, results, model switches — all events. UIs, JSON output, and plugins are subscribers to the same stream.
- **The session log stores the canonical message format, never a provider's.** Every request, the provider adapter transcodes canonical history → provider wire format from scratch (stateless transcoding). Mid-session model swap = next request uses a different transcoder. No migration step.
- **Provider-specific opaque data (reasoning/thinking blocks, encrypted reasoning items) is stored as provider-tagged attachments** on canonical messages: replayed verbatim to the same provider, dropped when crossing providers. Tool-call IDs are internal; each transcoder maps deterministically to provider-compliant IDs. Prompt-cache markers are injected at transcode time, never stored.
- **Model refs are `provider/model`** plus user-defined aliases (`fast`, `smart`) from config. Context-window metadata comes from the curated static `modelmeta` table. It never refreshes over the network.
- **A history repair that runs on live or persisted state is additive-only.** `LoadSession` writes the repaired slice back into live history, so a repair that deletes loses data permanently — not for one request, but for the life of the session. Add synthetic parts; never drop, reorder, or relocate a part another producer wrote. A transcode-time repair MAY be destructive, because it builds one throwaway request and never touches the record. Put every destructive rule on that side of the line. (Incident: a `ResolveOrphanToolCalls` rewrite deleted genuine tool output in three shapes and was reverted; see NEP-5293.) The concrete split is in "Wire normalization" below.
- **An empty tool result must never serialize as `null`.** The provider reads a null-content `tool_result` as ABSENT and rejects the whole request with "tool_use ids were found without tool_result blocks immediately after" — naming a block that IS in the payload. A tool that produces no output (a `grep` that matches nothing) is enough to wedge a session forever. `message.NoToolOutputText`, `ToolResult.SafeContent`, and `ToolResult.MarshalJSON` hold this line; every transcoder reads through `SafeContent`, never `Content`. (Incident: NEP-5272.)

## Wire normalization

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

## Ambient engine context is a structured, unforgeable part

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
per-request copy, never the durable log, so prompt-cache-prefix and persistence
rules stay unchanged) but still round-trips through the
canonical JSON union like every other part. Never revert this to a `Text`
part, and never make the guidance trust bracketed text syntax again. (Fix:
the NEP ambient trust-spoofing finding; superseded PR #113's prose-only
stopgap.)

## Appended system prompt (`append_system_prompt`)

Config key `append_system_prompt` is an array of environment facts. Use it for
facts an agent cannot discover, such as a gateway URL or required bind address.
Do not use it for tool instructions or project instructions.

The engine places entries after `Config.System` and before its generated
segments. `serve` and `run` both set `Config.AppendSystemPrompt`. For `run`,
configured entries come before the `-system` value.

The merge is additive. User-config entries come first, then project-config
entries. Other config slices replace the user value. This field differs because
the user file can belong to the platform while `.harness.json` belongs to the
cloned repository. Replacement would remove platform environment facts.

`serve` loads config once from its process working directory. A session with a
different working directory still receives that process config. Harness does
not load another `.harness.json` for each session.

Claude Code receives one blank-line-joined `--append-system-prompt` value.
`Config.System` is not forwarded because it describes native Harness tools.
Claude Code keeps only the last repeated prompt option. Therefore, config
validation and the engine reject `--append-system-prompt` and
`--append-system-prompt-file` in `ExtraArgs` when this key is present. Without
this key, `--append-system-prompt` remains a legacy `ExtraArgs` escape hatch.

The joined value crosses the operating system argument boundary. Keep these
environment facts short. The operating system can reject an unusually large
argument; Harness cannot derive one portable limit because the environment and
other arguments share that limit.

## Project instructions (AGENTS.md)

The engine injects a project's `AGENTS.md` into the system prompt. The first
load normally happens during `Prompt`. An eligible fresh session starts the
same load during background startup prewarm. Loaded sessions and sessions whose
provider is not eligible remain lazy until `Prompt`. The engine walks up from `Config.WorkDir`
for `AGENTS.md` (falling back to `AGENT.md`), stopping at the git root or
filesystem root; the closest file wins, per the
[agents.md](https://agents.md/) convention. The file is schema-less Markdown —
no headings are required or parsed. The segment is appended after
`Config.System` and before hook (`system.transform`) segments, cached for the
session, and never written to the session log (loaded fresh on resume).

A present-but-unusable file (invalid UTF-8, or empty/whitespace-only) fails the
first `Prompt` — a project that meant to supply instructions must not run
silently without them. A missing file is fine. Disable with
`-no-instructions`, config `instructions: false`, or point at a specific file
with config `instructions_path`.

An oversize file is truncated, and the truncation is LOUD on both channels.
`truncateInstructions` (`engine/instructions.go`) appends the in-band marker
`formatTruncationMarker` builds — it names the path, the original size, the
kept size, the dropped size, and the `read_file` tool that reads the rest —
and writes one WARN log line with the same counts. A silent cut was the
earlier behavior: a 408 KiB `AGENTS.md` reached the model as 64 KiB with no
sign of the missing 344 KiB, so the model followed a half specification and
believed it read the whole one. A truncated instruction file must always
announce itself; never make this cut quiet again.

`InstructionsConfig.MaxBytes` sets the cap: 0 (the zero value) takes
`defaultMaxInstructionsBytes` (64 KiB), a positive value sets it, a NEGATIVE
value disables the cap so the whole file is injected. Config key
`instructions_max_bytes` (bytes) and the operator seam
`HARNESS_INSTRUCTIONS_MAX_KB` (kilobytes; negative disables) resolve it in
`cmd/harness`, the environment variable winning — the engine never reads an
environment variable itself.

An oversize file is not merely marked, it is SPLIT.
`renderInstructions` (`engine/instructions_outline.go`) injects a HEAD plus an
OUTLINE: the head is every section that fits whole under the cap, and the
outline lists every section the head does not carry — one line each with the
heading, the exact `read_file` range that reads it
(`read_file(path=<abs>, offset=<line>, limit=<count>)`), and a short teaser
from the section body. The model pulls a section with `read_file`, whose
`offset`/`limit` are already 1-based line numbers, so this adds NO tool and no
schema to any request. The shape is the Agent Skills stage-1/stage-2 split
below, applied to one file: the outline is an index the model MUST read
through before it relies on a section. The former monolithic root exceeded
the cap, so its head carried the sections that fit and its outline advertised
every later section. Nothing was out of reach, where the marker alone had
left the whole tail unreachable.

`scanSections` tracks fenced code blocks by FENCE CHARACTER AND RUN LENGTH,
not with a boolean. A `#` comment inside a ```` ```bash ```` block read as a
heading would advertise a range that points at a shell comment. The character
and run-length rules cover the next shape up: an instruction file that
documents Markdown wraps a three-backtick example in a four-backtick fence,
which a naive toggle closes early, turning the rest of the document into
false sections.

The split composes with the cap in ONE direction: the cap decides what is
EAGER, the outline makes everything else REACHABLE, and nothing is dropped in
silence either way. Three shapes keep the marker. A file with fewer than two
sections (no heading, or one giant section) has nothing to outline and takes
the `truncateInstructions` path unchanged. `InstructionsConfig.Mode`
`InstructionsModeFull` selects that path for every file. And a file whose
FIRST section alone exceeds the cap has no boundary to cut on, so the head is
that truncated first section — marker and WARN line both firing, through
`truncateInstructionsOf`, which reports the WHOLE file's size and not the
first section's — with the outline still listing every later section. Never
let the head lose its marker in that shape: it is the one place where an
outline could hide a cut. Config key `instructions_mode` and the operator seam
`HARNESS_INSTRUCTIONS_MODE` resolve the mode in `cmd/harness`; only the value
`full` (case-insensitive) turns the outline off, because an unreadable knob
must not quietly drop an outline nobody asked to lose. Design:
docs/design/nested-instruction-loading.md.

## Agent Skills

The engine advertises [Agent Skills](https://agentskills.io) in the system
prompt following the spec's progressive-disclosure model. Discovery normally
runs on the first `Prompt`, alongside instruction loading. An eligible fresh
session starts both load-once operations during background startup prewarm.
Loaded and ineligible sessions remain lazy until the first prompt. The engine runs `skill.Discover` over each
configured directory, merges the results sorted by name, and injects one system
segment **after** the instructions segment and before hook (`system.transform`)
segments. That segment is stage 1 only: a header telling the model it MUST read
a skill's `SKILL.md` with the `read_file` tool before relying on it, then one
line per skill — `name — description (path: <abs SKILL.md>)`. Stage 2 (the body)
is deferred to that read.

`Config.SkillsDirs` selects the directories: nil (the default) uses
`<WorkDir>/.agents/skills` when it exists; an explicit empty slice disables
discovery. A malformed `SKILL.md` or a duplicate skill name across dirs fails
the first `Prompt` loudly (same semantics as a malformed AGENTS.md). Skills are
never written to the session log — a resumed session rediscovers them. Config
`skills_dirs` (array; a non-empty project value overrides the user value
entirely) and the repeatable `-skills-dir` run/serve flag drive it.

## Tool-batching guidance

The engine executes one assistant message's tool calls concurrently
(`engine/toolexec.go`, capped by `Config.ToolConcurrency`), but a model
that emits one call per turn never produces a batch wider than one. The
executor is only as useful as the model's willingness to batch, so the
engine asks for it: `toolBatchingSegment` (`engine/toolexec.go`) injects
one system segment telling the model to put independent calls in the same
message, and to wait when a call's arguments depend on an earlier call's
result. Both halves matter — the second is what stops a model
parallelizing genuinely dependent work, which no amount of executor
correctness can repair.

The segment sits immediately after `Config.System` and before the
instructions segment (`engine/engine.go`): it describes how this engine
runs tools, not anything about the project. It is **gated on the
session's resolved concurrency and is empty at 1** — an operator who set
`HARNESS_SEQUENTIAL_TOOLS=1`, or an embedder who set `ToolConcurrency: 1`,
must not be told calls run concurrently when for that session they do not.
The cap in the text is rendered from `s.toolConcurrency`, so the number
the model reads is the number the executor enforces. Like every other
engine-injected segment it is never written to the session log.

Adding a base segment shifts every later segment's index, so the
segment-layout assertions across `engine/*_test.go` pin it explicitly via
`isBatchingSegment` (`engine/toolbatching_test.go`); only that file pins
the wording itself.

## read_file image support

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
codebase to gate it on — `modelmeta` carries context-window data but no vision
capability, and
`provider.Request` carries no capability flag comparable to `Effort` or
`SessionKey` that a caller could set from one. Building this now would mean
inventing an ad hoc, likely-wrong static model list, so it is deferred to
issue #129. Until it lands, a model with no vision support receives the
image Blob exactly as any vision-capable model does; how it handles that
block is between the model and its provider.

## Bounding concurrent-read memory

`read_file`'s text path is deliberately unbounded (`readPathContent`,
`engine/filetools.go`) — a coding agent legitimately reads whole files, and
no byte cap is right for every file. Only the IMAGE path is capped
(`readFileMaxImageBytes`), and `bash` caps its own output
(`defaultBashOutputCap`).

That was safe while tool calls ran strictly one at a time: peak heap held
at most ONE file's raw bytes plus the line-numbered copy built from them.
The concurrent executor (`engine/toolexec.go`) removed that implicit bound
without replacing it, so a batch of N large reads holds N working sets at
once. Measured with eight 16MB files, retention swallowing the finals so
only the transient term shows: **~325MB peak parallel against ~73MB
sequential — ~4.3x, bounded only by `ToolConcurrency`**, with the model
choosing both the batch width and the file sizes.

`toolReadBudget` (`engine/toolmem.go`) is the replacement bound. Each
`read_file` reserves its file's Stat size against a per-session byte
budget before touching the file, and holds it until the call returns —
spanning the line-numbering too, since for a large file `strings.Split`
is the single biggest allocation and releasing after the read would leave
the expansion outside the bound. With the budget set to one file's size
the same batch peaks at the SEQUENTIAL figure: ~73MB, **1.0x**. It bounds the **product** of read size and
concurrency, which is the actual hazard: a count limit still admits two
500MB reads, and a size limit breaks the legitimate large read. Ordinary
work never contends — a full-width batch of kilobyte reads reserves a
rounding error against the default and stays fully parallel
(`TestReadBudgetKeepsSmallReadsFullyParallel`).

It bounds the TRANSIENT working set, not the ACCUMULATED results:
`runToolBatch` holds every call's output until the join in BOTH execution
modes, so N results occupy the same memory however they were produced.
That term is not a concurrency regression, and retention already collapses
oversized results where configured.

Three properties keep it safe. A worker takes its pool slot first and then
reserves, which is head-of-line blocking but cannot deadlock: only a slot
holder ever holds budget, so someone is always making progress, and a
reservation is never held while acquiring another. A read larger than the
whole budget is CLAMPED to it, not refused, so it waits for the budget to
drain and runs alone — a batch always progresses. Waiters are served
strictly FIFO, because a retry-when-there-is-room loop lets a stream of
small reads starve one large read forever.

`Config.ToolReadBudgetBytes`: 0 (the zero value) takes
`defaultToolReadBudgetBytes` (64 MiB), a negative value DISABLES the bound,
a positive value sets it. `HARNESS_TOOL_READ_BUDGET_MB` is the operator
seam (megabytes; negative disables), resolved in `cmd/harness` like
`HARNESS_TOOL_CONCURRENCY`. The budget is per SESSION: a process running
many sessions bounds each, not their sum — a process-wide budget is the
natural follow-up if that proves insufficient.

## write_file read-before-overwrite guard

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

Tool calls in one assistant batch execute concurrently. `filePathKey` gives
`read_file`, `write_file`, and `edit_file` calls on the same resolved path one
key, and `keyChain` runs those calls in FIFO order. Preserve that exclusion:
the read-set map's per-operation mutex does not make the
check-current-hash-then-write sequence atomic by itself.

`bash` writes (a model redirecting output to an existing file via a shell
command) are explicitly OUT of scope: harness cannot classify an arbitrary
shell command as a file write versus anything else it might do, so this
guard covers only the two built-in tools that make a structured, typed
claim to write a file.

## Base loop retry

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
and SHORTER than the goal loop tiers in `docs/goal-loop.md` — an interactive user waits on
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

## Max-tokens auto-continue

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

## Startup prewarm

Fresh native sessions can prepare provider-owned transport state before the
first prompt. `NewSession` assigns the session ID and returns without waiting.
Managed roots start only after adoption and task-tool installation. Spawned
children start only after their lineage, model, agent type, and tool restrictions
are final. Loaded sessions, goal evaluators, compaction summaries, and the
Claude Code delegated path do not schedule startup prewarm.

The engine schedules the task only when the initially configured provider
implements `provider.StartupPrewarmer` and its side-effect-free
`StartupPrewarmEnabled` method returns true. The eligibility check runs before
instruction and Skill discovery, hooks, MCP access, or tool assembly. The task
then performs the same stable-prefix assembly as a real turn:

1. Load and cache project instructions.
2. Discover and cache the Agent Skills catalog.
3. Run `chat.params` and resolve the effective provider.
4. Build the effective built-in, MCP, and plugin tool plan.
5. Build ordered system segments and run `system.transform`.
6. Build an empty-message `provider.Request` with the session key.
7. Call the effective provider's `Prewarm` method if it still has the capability.

This is an early disclosure boundary for eligible fresh sessions. Disk reads,
hooks, MCP connection attempts, plugin activity, and provider work can start
after fresh-session construction and before user input. The OpenAI client is
eligible only when its family is `codex` and WebSocket transport is enabled.
Generic OpenAI and HTTP-only clients remain lazy until the first prompt. A
qualifying Codex call connects the session-keyed socket and sends an empty-input
`response.create` with `generate:false`. It keeps `store:false` and waits for
`response.completed`.

The prewarm request contains the stable system and tool prefix. It can disclose
base and appended system segments, project instructions, the Agent Skills
catalog and local paths, tool names and schemas, a deferred MCP catalog, model
controls, and the session ID as `prompt_cache_key`. It contains no user prompt,
transcript, tool result, or ambient first-turn status. Ordinary OpenAI requests
still reject an empty transcodable message set.

One 15-second context covers the whole task from scheduling: discovery, hooks,
tool and MCP assembly, dial, send, and completion. The first native `Prompt`
consumes the handle once before context validation, cached discovery-error
checks, compaction, or user-history append. If work is pending, the prompt waits
only for the original deadline's remainder. It does not start another timeout.
A ready compatible Codex lineage lets the first real call send only its new
input. The real turn still assembles and validates its request; any mismatch
sends a complete request.

A prewarm failure or deadline does not fail the prompt. The engine detaches the
handle and the normal call proceeds. Prompt-context cancellation cancels the
prewarm and returns that cancellation before history mutation. Session removal
also cancels an owned task.

`StartupPrewarmer.Prewarm` must return promptly after context cancellation. The
engine bounds prompt waiting and session ownership independently: a deadline
signal cancels the worker and detaches the handle before an outcome callback can
block. The first winning outcome is committed before callback invocation, so a
reentrant callback cannot change that outcome or deadlock its once gate. Go
cannot terminate an arbitrary in-process callback. A provider that ignores
cancellation can therefore leave one residual, unowned callback goroutine
blocked after the 15-second boundary. The engine does not retain it, wait for
it, or let it delay the first prompt. Providers must obey the cancellation
contract to prevent that residual limitation.

Prewarm emits no provider events, user or assistant messages, usage, or
`turn_metrics`. It emits `startup_prewarm` lifecycle records through
`Config.OnStartupPrewarmMetrics` or the default structured stderr sink. Statuses
are `started`, `ready`, `consumed`, `failed`, `timed_out`, `cancelled`, and
`stale`. Each record contains `session_id`, `duration_ms`, and `age_ms` without a
provider response ID. `consumed` and `stale` report age when the first completed
request resolves whether it used compatible prewarm lineage. A full request or
a chain-miss recovery makes a ready prewarm `stale`.

Deterministic instruction and Skill discovery errors remain cached and fail the
first prompt through the normal checks. Startup-only provider, hook, MCP, and
transport failures remain best effort; the normal turn reports only failures it
encounters itself.

## Per-turn metrics

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

An adapter can attach transport projection metadata only to `EventDone`. When
present, `turn_metrics` also includes `request_mode`, `complete_input_items`,
`sent_input_items`, `previous_response_used`, and `chain_recovered`. Codex
WebSocket calls report `request_mode` as `full` or `incremental`. An immediate
chain-miss recovery reports the final complete retry as `full` and sets
`chain_recovered=true`. HTTP and adapters without projection metadata omit these
fields. Startup prewarm emits no `EventDone` and therefore has no `turn_metrics`
record; its separate lifecycle metric is described above.

Usage remains the completed response's provider report. For OpenAI Responses,
`input_tokens` on the wire includes `input_tokens_details.cached_tokens`.
`provider/openai` converts it to disjoint `provider.Usage` values: cached input
becomes `CacheReadTokens`, and `InputTokens` is the non-negative uncached
remainder. Chaining never estimates cache hits from item counts or request mode.

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
