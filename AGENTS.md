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

### Goal loop

`Session.PursueGoal(ctx, condition, GoalOptions)` drives the ordinary `Prompt`
loop toward a natural-language completion condition. Turn 1 prompts the raw
condition; after **every** turn an independent, TOOL-LESS evaluator model
(`GoalOptions.Evaluator`, resolved through the same `Config.Providers` registry,
`MaxTokens` 256) is asked to answer `MET: <reason>` / `NOT MET: <reason>`
(parsed leniently). A NOT MET verdict re-prompts with a fixed-template guidance
message carrying the reason; MET returns `Achieved`. `MaxTurns` (0 = unlimited)
bounds it. Evaluation is advisory: a retryable-class provider error from the
evaluator call rides the existing retryable backoff schedule in-boundary
before the boundary counts as failed; two unparseable replies in a row (the
second re-asked with a stricter prompt) or a non-retryable provider error also
fail the boundary immediately. A failed boundary no longer clears the goal —
it journals a durable `goal.eval_failed` record (carrying the consecutive
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

A worker-turn error (`s.Prompt` failing) is retried by `promptTurnWithRetry`:
`goalWorkerRetries` (2) additional deterministic attempts (~5s total), or,
for a provider error classified `provider.AsRetryable`, a separately
budgeted `goalRetryableMaxAttempts` (12) backoff (~30min total) that never
spends the deterministic budget — recording a `goal.stalled` record for
every failed attempt either way, so the loop is never silent. Exhausting
EITHER budget — or the non-idempotency gate stopping retries early once a
tool has already executed this attempt — now PARKS the goal instead of
clearing it: `PursueGoal` exits, journals a durable, CLASSIFIED `goal.parked`
record (never raw provider error text — the same leak rule `goal.eval_failed`
follows), and returns a distinct `*goalWorkerParkedError` sentinel
(`engine.IsGoalWorkerParked`) WITHOUT calling `clearGoal` — `goalActive`
stays true, the condition is untouched, generation-gated exactly like
`goal.stalled`/`goal.eval_failed` so a park racing a concurrent `UpdateGoal`
is silently discarded rather than attributed to a condition the model never
saw. This supersedes both this package's earlier deterministic-tier clear
and GitHub issue #61's in-loop retryable-tier self-re-arming `continue` — the
latter pinned the run slot to the parked loop for the whole outage; exiting
instead frees the slot, so a queued prompt dispatches as an ordinary turn
during a long outage instead of only ever being injected mid-turn into a
doomed attempt. Context overflow (issue #62) is the one deliberate exception
and still clears immediately, never parks: no amount of waiting fixes an
oversized request, so parking it would just be a slower-burning zombie
instead of a fix. Parking has no streak horizon (unlike the evaluator's
5-boundary terminal above) — every exhaustion parks immediately, and
`DELETE /session/{id}/goal` remains the only clear path for a parked goal.

On the server, a worker-parked sentinel maps to `session.error` plus a
distinct `turn.end outcome=worker_parked`, and `goalTracker` folds the
durable `goal.parked` record into a third `paused` arm (`pause_reason:
"worker_failure"`, alongside the existing boot-only `"restart"` and live
`"provider-backoff"`) — `compositeState` forces `idle` for it exactly like a
restart pause, since no loop is actually driving the goal, unlike
provider-backoff, whose loop is merely waiting and keeps reading
`goal-running`. Resume needs no new machinery: the existing activity-driven
`maybeAutoArmGoal` re-arms any active goal — parked or not — the next time an
ordinary prompt turn completes, resetting the `worker_failure` presentation;
`runGoal`'s own tail deliberately never auto-arms (the same anti-churn
property that already stops a freshly-parked goal from immediately
respawning a loop against an empty queue).

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
...]` status block to the newest user message ONLY — never persisted into
the durable session log, never touching any earlier message so a
provider's prompt cache prefix stays intact. See
`docs/design/managed-processes.md` §4 for the exact mechanism and why it
is safe.

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

**Hub spawn contract:** the hub that spawns boxes — `harness hub`, now
implemented in `tools/hub/` (see the Development hub above) — passes the
generated box NAME to the spawn command's environment as
`HARNESS_HUB_BOX_NAME`, so deployment scripts can derive per-name storage
(e.g. mount/create a volume named after it) without the hub and the box
needing any other side channel to agree on identity. Harness itself never
reads this variable — it is a contract between the hub and deployment
tooling, documented in `docs/design/fleet-model.md` §8.

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
- **Exception — cross-process e2e** (`e2e/` only): tests driving a real
  subprocess may observe out-of-process state with deadline-bounded poll
  loops, because no in-process channel can cross an OS process boundary.
  Intervals stay tight, deadlines explicit; anything observable in-process
  still uses channels or synctest.
- **No raw `time.Sleep` for synchronization — ever, bubble or not.** To
  simulate a hung component, block on a channel closed in `t.Cleanup`; in a
  bubble the hang deterministically outlasts any timeout with zero wall-clock
  cost, and the cleanup release lets the goroutine exit before bubble end.
- **No guessed deadlines.** Block directly on channels for expected events
  and let the test binary timeout catch hangs; don't wrap waits in short
  arbitrary `time.After` failsafes that flake under load.
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

## Git Commits

- [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): description` (e.g. `feat(plugin): add shell.env hook`).
- Do not include `Co-Authored-By` lines for AI agents in commit messages.
