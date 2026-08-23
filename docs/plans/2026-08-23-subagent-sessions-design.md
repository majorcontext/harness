# Subagent sessions

Status: design approved in conversation (Andy, 2026-08-23); this document is
the written spec for review before implementation planning.

## Motivation

harness has no way to delegate work to an isolated context. Every
exploration, review, or fan-out lands in the main session's context, and
compaction is the only relief valve. Competing harnesses (Claude Code,
opencode, fx) all ship a subagent primitive, and models are trained to
reach for one. Separately, the boxes platform cannot clear a session or
create a new one programmatically — the console has no "new session"
operation.

Both gaps share one root: harness has exactly one session per process and
no session lifecycle API. This design fixes the root, and subagents fall
out as a thin layer on top.

## Locked decisions

These were decided explicitly and are not open for re-litigation during
implementation:

1. **One primitive.** A subagent IS a child session. Session management
   (create/send/inspect) is the platform-facing feature; the `task` tool is
   model-facing sugar over it. "Clear" in a UI = create a fresh session and
   repoint.
2. **Non-blocking execution.** Spawning returns immediately. The parent
   keeps accepting prompts while children run. opencode's blocking model
   (parent turn waits for children) is explicitly rejected.
3. **Completion delivery — queue-or-resume.** A child's completion is
   delivered at the parent's next turn boundary if a parent turn is
   streaming; if the parent is idle, the engine initiates a resume turn
   itself. (Claude Code's observed model.)
4. **Presets match Claude Code.** Built-in agent types `general-purpose`
   (full tool set), `explore` (read-only), `plan` (read-only, returns a
   plan). Custom agent definitions load from `.agents/agents/*.md` with
   Claude Code-compatible frontmatter.
5. **Nesting matches Claude Code.** Children can spawn children by
   default. Depth limit 3 below the root session (configurable); at the
   limit the `task` tool is withheld. Tree-wide running-children cap
   (default 20, configurable). Any agent definition can exclude `task`
   to make its type a leaf.

## Architecture

### SessionManager

A new `engine.SessionManager` owns every live session in a harness
process. Today's single-session flow becomes the degenerate case: one
root session, zero children.

    type SessionManager struct {
        // sessions by id; the root session is the one with no parent.
        // Children hold ParentID and Depth (root = 0).
    }

Responsibilities:

- Create sessions (fresh roots or children), applying an agent
  definition's tool filter, model override, and prompt addition.
- Track lifecycle: `running` (turn streaming), `idle`, `done` (terminal
  result produced), `failed`, `canceled`.
- Enforce depth and concurrency limits at spawn time.
- Cascade cancellation: canceling a session cancels its subtree.
- Route completion notifications to parents (see Delivery).

Each child session is a real `engine.Session`: own context, own journal
/ session log (persisted under the existing session-log layout with the
parent id recorded), own tool list. Children inherit the parent's
provider, system prompt (fx-style — no distinct subagent persona), and
working directory. The agent definition may append to the system prompt.

### Agent definitions

Built-ins (compiled in):

| name              | tools                                        | notes |
|-------------------|----------------------------------------------|-------|
| `general-purpose` | parent's full tool set (incl. `task`)        | default |
| `explore`         | read-only: `read_file`, `glob`, `grep`, `ls` | leaf (no `task`) |
| `plan`            | read-only set; prompt addition instructs it to return an implementation plan, not edits | leaf |

Custom definitions: `.agents/agents/<name>.md` (sibling of the existing
`.agents/skills/` convention), Claude Code-compatible frontmatter:

    ---
    name: code-reviewer
    description: Reviews diffs for correctness and convention adherence
    tools: read_file, glob, grep        # omit for parent's full set
    model: <model-id>                    # optional override
    ---
    <body: appended to the child's system prompt>

Loaded once at root-session start (same lifecycle as skills discovery).
A custom definition whose `tools` list omits `task` is a leaf. Unknown
tool names in a definition are an error surfaced at load, not spawn.

### The `task` tool (model-facing)

One built-in tool, registered on any session whose depth is below the
limit and whose agent definition includes it:

    task(agent: string, prompt: string, model?: string) -> {session_id}

- Returns immediately after the child session is created and its first
  turn is enqueued. The result contains the child session id and a
  reminder that the result arrives later as engine context.
- Errors at call time: unknown agent name, depth limit reached (tool is
  withheld at the limit, but a race is still answered with an error, not
  a crash), tree concurrency cap reached.
- Optional `model` overrides the definition's model, which overrides the
  parent's.

### Completion delivery (queue-or-resume)

When a child reaches `done` or `failed`, SessionManager enqueues a
completion notification on the parent:

- Parent `running`: the notification is injected at the parent's next
  turn boundary.
- Parent `idle`: the engine initiates a resume turn on the parent with
  the notification as its trigger. This is a new engine capability —
  engine-initiated turns — and reuses the goal loop's existing "the
  engine may act after a turn" precedent.

Notifications ride the existing **EngineContext part** (unforgeable,
appended to the newest user-role message, never the system prompt) —
consistent with harness's trust model and prompt-cache design. The
child's final text is included as data inside the engine-context
envelope; anything instruction-shaped in it is the model's to distrust
per the existing sentinel rule.

The notification carries: child session id, agent type, status
(done/failed), the child's final message text, and usage totals.

### Wire / platform API

Three operations, exposed wherever harness already exposes session
operations (the boxes control plane is the first consumer):

- `session.create` — `{parent_id?, agent?, model?, prompt?}` → session
  id. With no parent: a fresh root (this is "new session"/"clear" for
  the boxes console). With a parent: identical to a `task` call made
  from outside the model.
- `session.send` — deliver a user-role message to any session by id
  (root or child).
- `session.info` — status, lineage (parent id, depth, children), usage,
  and — for `done` sessions — the final result text. Extends the
  existing `session_info` surface rather than duplicating it.

### Limits and failure

- Depth: default 3 below root; config `HARNESS_MAX_TASK_DEPTH`. Mirrors
  Claude Code's semantics: at the limit, `task` is not registered.
- Concurrency: default 20 running children per tree; config
  `HARNESS_MAX_CONCURRENT_TASKS`. Counted tree-wide from the root, not
  per level.
- A child that errors terminally (provider failure after retries, tool
  crash, cancellation) delivers a `failed` notification with the error
  classified through the existing model-visible-string rules. It never
  crashes the parent.
- Canceling a parent cancels its entire subtree before the parent
  finalizes.
- Child session logs persist like any session's, so a child's work is
  inspectable after the fact.

## Non-goals (v1)

- Streaming child transcripts into a UI live (boxes follow-up; the
  console's lineage vocabulary from the fleet-rail work is the intended
  rendering surface).
- Cross-child messaging (fx's `message`/`relationship` actions).
- Distinct child personas beyond the definition's prompt addition.
- Any change to the boxes web console (separate repo, separate PR, after
  this ships).

## Testing strategy

- SessionManager unit tests: lifecycle transitions, depth/concurrency
  enforcement (including the at-limit race), cascade cancel, notification
  routing (running-parent queue vs idle-parent resume).
- Agent-definition loading: built-ins, `.agents/agents` parsing, unknown
  tool names error at load, `tools` filtering applied to the child's
  registry, leaf types get no `task`.
- Delivery integration test: parent idle → engine-initiated resume turn
  carries the EngineContext notification; parent mid-turn → delivery at
  the next boundary; multiple children completing during one parent turn
  arrive as multiple parts in one boundary.
- E2E (monitor/e2e pattern): root spawns `explore` child, keeps
  accepting a user prompt while the child runs, receives the child
  result, chains a second spawn from the resume turn.
- Wire tests: `session.create/send/info` including create-with-parent
  equivalence to `task`.

## Rollout

1. Engine: SessionManager + child sessions + `task` + delivery (this
   spec's implementation plan).
2. Wire surface for `session.*`.
3. boxes: console "new session" + child-session rendering (separate
   design, separate repo).
