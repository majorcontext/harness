# Engine instructions

These rules apply to `engine/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root.

Read `message/AGENTS.md`, `provider/AGENTS.md`, or `server/AGENTS.md` when a
change crosses those boundaries.

Detailed behavior and rationale live in `docs/`. This file contains edit-time
constraints for the engine.

## Read first

- Request assembly, tools, retries, or metrics:
  `docs/engine-request-cycle.md`.
- Goal supervision: `docs/goal-loop.md`.
- Journals, indexes, pages, queues, or processes:
  `docs/session-storage-and-queue.md`.
- Models, effort, affinity, or provider caches:
  `docs/models-and-providers.md`.
- Task lineage or provider exhaustion: `docs/design/fleet-model.md`.
- Compaction: `docs/design/context-compaction.md`.
- Project instruction loading:
  `docs/design/nested-instruction-loading.md`.
- MCP schema deferral: `docs/design/mcp-lazy-tools.md`.
- MCP connection recovery: `docs/plugins-and-protocols.md`.
- Process lifecycle: `docs/design/managed-processes.md`.

## Sessions, history, and ambient context

- Treat the session log as append-only.
- Store canonical `message.Message` values, not provider wire values.
- Keep live and persisted history repairs additive-only.
- Hold `Session.mu` while persistence and emission must form one observation.
- Keep runtime-only state out of the journal unless replay needs it.
- Pass the firing session ID into callbacks copied into child sessions.

Only engine code creates `message.EngineContext`. Use it for trusted ambient
status and continuation nudges. Never replace it with `message.Text` or persist
it. Add ambient status only to a throwaway request copy.

## Project instructions and Agent Skills

`loadInstructions` searches upward from `Config.WorkDir`. It returns the first
`AGENTS.md` or `AGENT.md` and stops at the Git root or filesystem root. It runs
on the first prompt, not at `NewSession`.

- Treat a missing instruction file as valid.
- Reject an empty or invalid UTF-8 instruction file.
- Make truncation visible in the prompt and logs.
- Keep omitted sections reachable through the generated outline.
- Do not claim that nested attach-on-read is implemented.

Discover Agent Skills on the first prompt. Inject only the catalog. Require the
model to load a selected `SKILL.md`. Reject malformed or duplicate skills.
Rediscover skills after resume and never persist the catalog.

## Tool execution and file tools

- Run independent calls concurrently up to `ToolConcurrency`.
- Preserve serial barriers and original result order.
- Run calls with one non-empty key in call order.
- Use resolved file keys for `read_file`, `write_file`, and `edit_file`.
- Do not let a keyed waiter hold a worker slot.
- Return exactly one result for every tool call, including cancellation.
- Keep the model-facing batching segment equal to resolved concurrency.
- Do not claim that file keys cover Bash side effects.

Keep the tool array byte-stable. Sort built-ins by name. Preserve built-in, MCP,
then plugin group order. Make every new tool source deterministic.

Classify images by magic bytes from one open handle. Enforce the 20 MiB limit
and inspect dimensions through that handle. Keep non-images on the text path.

Reserve `read_file` memory from the stat size before reading. Serve reservations
FIFO. Let one oversized read use the full budget alone.

Before overwriting an existing regular file, require a successful live-session
read or write record. Compare its saved SHA-256 with current bytes. Track
resolved absolute paths and serialize parallel mutations by file key.

## Requests, retries, and metrics

- Retry only typed retryable errors and completed empty turns.
- Never retry cancellation, permanent request errors, or interrupted tool intent.
- Emit `EventTurnRestart` before retrying after partial streamed output.
- Do not journal a failed attempt or run its tools.
- Keep interactive backoff shorter than goal-worker retry tiers.

Treat `StopMaxTokens` as incomplete. Continue within one `Prompt` and one
`MaxTokensContinuations` budget. Preserve a partial tool call's identity,
drain operator input first, and end with a user-role engine-context nudge.
Mark budget exhaustion permanent.

Emit one `TurnMetrics` record per completed provider call. Do not emit one for
a failed or interrupted stream. Preserve server join fields. Inject `Config.Now`
in tests.

## Goal supervision

- Keep the evaluator tool-less and force `message.EffortOff`.
- Bound evaluator context independently from main-model context.
- Select retry tiers with typed error classes.
- Clear on context overflow. Park on worker retry exhaustion.
- Persist goal transitions and generation-check stale results.
- Reuse one unanswered directive across retries.
- Keep the `goal` tool free of a `clear` action.
- Require operator evidence for completion.

## Persistence, queues, and processes

Treat the sidecar index as a cache of the journal fold. Validate its checksum,
journal size, and modification time. Refold on doubt. Share fold logic with
full replay.

Treat a snapshot as a versioned checkpoint, never as authority. Capture only
at a durable append boundary. Reject capture while `durableDebt` is non-zero.
Fall back to full replay for every invalid snapshot.

Number `MessagePage` values by durable folded message sequence. Keep the legacy
unparameterized response as a full array. Never add synthetic orphan repairs
to a page. Bound scans to the indexed log size.

- Keep the prompt queue durable and FIFO.
- Persist enqueue before acceptance and dequeue before delivery.
- Let queued input beat goal auto-arm.
- Deliver each item once across tool and goal-turn drains.
- Do not auto-dispatch a restored queue at boot.
- Keep queued prompts text-only and model-override-free.
- Preserve durable sequence deduplication and its high-water mark.

Keep the process manager box-scoped and shared. Runtime declarations are not
configuration writes. Preserve process-group termination and asynchronous exit
detection. Render status only as runtime `EngineContext`.

## Children, models, compaction, and effort

- Persist child lineage, depth, agent type, and spawn links.
- Keep parent notifications durable and bounded.
- Preserve provider-exhausted children for later resume.
- Mask and cap provider failure details.
- Resolve reaped descendants through durable ancestry.
- Run slow disk recovery outside the manager-wide lock.

Use `Session.SetModel` and `Session.SetEffort` as the only change choke points.
Persist and emit real changes; write nothing for no-ops. Re-evaluate a derived
context window after a model switch. Refuse a required registry miss.

- End summarization requests with a new `RoleUser` instruction.
- Never send a folded assistant message as provider prefill.
- Detect summaries by structural message-ID prefix.
- Keep empty completed summaries as non-mutating no-ops.
- Keep failed summarizer calls as errors.

The main turn uses session effort. The evaluator uses `EffortOff`. The
summarizer inherits session effort. Do not combine these policies.

## MCP

- Keep first connection lazy and bounded by the configured timeout.
- Park after the bounded background retry schedule.
- Let only explicit `mcp connect` revive a parked server.
- Read live state when publishing tools.
- Serialize background and explicit connections per server.
- Show only classified, secret-safe errors in runtime `EngineContext`.
- Never defer schemas without the `mcp` selector tool.
- Sort the complete catalog by full tool name.
- Resolve the provider before a plan can dial MCP.
- Keep selections durable and reap them only after proved absence.

## Deliberately absent

The engine has no tool permission gate and no plan mode. Do not add either as
a local convenience.
