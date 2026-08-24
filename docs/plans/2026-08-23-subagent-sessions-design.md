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
   plan). Custom agent definitions load from `.agents/*.md` with
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

Custom definitions: `.agents/<name>.md` — loose `.md` files at the top
of the existing `.agents/` directory (skills stay under `.agents/skills/`;
only top-level `.md` files are agent definitions), Claude Code-compatible frontmatter:

    ---
    name: code-reviewer
    description: Reviews diffs for correctness and convention adherence
    tools: read_file, glob, grep        # omit for parent's full set
    model: <model-id>                    # optional override
    ---
    <body: appended to the child's system prompt>

Loaded once at root-session start (same lifecycle as skills discovery).
A custom definition whose `tools` list omits `task` is a leaf. Unknown
tool names in a definition are an error surfaced at load, not spawn —
never silently deferred to a later `task` call's own "unknown agent"
error at spawn time. A malformed or semantically-invalid `.agents/*.md`
file fails the WHOLE directory's load (a design-owner decision, after a
narrower "frontmatter leniency" follow-up first tried skip-and-warn for
every parseAgentDef error alike): the one exception is a stray unknown
frontmatter KEY (e.g. a typo'd `desc:` in place of `description:`),
which is judged low-stakes and cosmetic enough to skip just that one
file with a logged warning instead. An unknown tool name or an invalid
model string are not that — see engine/agentdef.go's
errUnknownFrontmatterKey doc comment for the full reasoning.

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

**Grandchild delivery — reparent to the nearest live ancestor.** A
child's own "parent" for delivery purposes is not always its immediate
spawner: a child that spawned its own grandchild typically finishes ITS
OWN turn (going `done`) before that grandchild does — the whole point of
non-blocking execution (decision 2) is that a parent never waits on a
child it spawned. When the grandchild later completes, its immediate
parent is already terminal (`done`/`failed`/`canceled`) and has no
notion of "next turn boundary" or "idle" to resume into. Rather than
stranding the notification, SessionManager walks up the tree past any
already-terminal node to the nearest ancestor still in `running` or
`idle` (queue-or-resume applies there, exactly as if that ancestor were
the direct parent) — reaching the root in the worst case, since a root
never goes terminal on its own (only via explicit cancellation). This
also applies to cancellation: a canceled node's own pending notifications
are forwarded the same way rather than discarded, so a subtree canceled
mid-flight never silently drops a result that had already arrived.

The alternative — "wake" the terminal parent back into a running turn
just to hand it a grandchild's result — was rejected: it would resurrect
a session the caller (or the parent's own agent) had already treated as
finished, with no way for anything downstream to distinguish that from a
genuine new turn on a session still doing work. Reparenting keeps `done`
a real terminal state for a child while still guaranteeing every
notification reaches SOME live node in the tree. Live-verified: a
general-purpose child that spawns an explore grandchild and finishes its
own turn before the grandchild does gets skipped, and the root — the
next live ancestor up — receives and correctly acts on the grandchild's
result instead.

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

### Process-restart recovery

The four terminal outcomes above (provider failure, tool crash,
cancellation, natural completion) all have one thing in common: a live
goroutine — somewhere in `finalizeTurn`'s call chain — is always the one
that discovers and reports them. A child whose turn was genuinely IN
FLIGHT when the process crashed or was killed has no such goroutine left
in the NEW process. Left unhandled, it cold-reloads as `StatusIdle` —
indistinguishable from a child that never received a turn at all — and
its parent, if it ever queries or auto-resumes based on that child's
outcome, waits forever for a notification that can never arrive.

**Detection.** `Session.Prompt` durably appends the user-role message
BEFORE calling the provider (see `runAgenticLoop`), so a crash mid-turn
leaves exactly one recognizable signature on disk: the session's history
ends on a user-role message with nothing after it. `hasUnansweredTurn`
(engine.go) checks for exactly this — the same signature
`isSafeToDropDirectiveTail`'s `len==1` case already trusts for a
different purpose (goal-loop retry).

**Decision: treat it as `failed`, synthetically, on next touch.** A
dangling child is marked `StatusFailed` (`FailReason`: `"lost to
restart: turn was in flight when the process last stopped"`) the moment
`adoptReloadedLocked` reconstructs it, and a synthetic notification is
delivered to its nearest live ancestor through the EXACT SAME
`nearestLiveAncestorLocked` + `enqueueTaskNotification` path every other
terminal outcome uses — never a second-class delivery mechanism. See
`recoverInterruptedTurnLocked`'s own doc comment (session_manager.go) for
the full mechanism, including why the resulting resume is fired
asynchronously rather than threaded back through `AdoptReloaded`/
`ReportTurnStart`'s public signatures.

**Accepted scope cut: reactive, not proactive.** This only fires when
something actually reloads the dangling child's own id again (a
legitimate follow-up `session.send`, `ReportTurnStart`'s
adopt-on-first-sight, or `handleSpawnChild`'s parent-lookup fallback all
already reach this path). If nothing ever touches that child's id again,
its parent still waits forever. Fully closing that requires a proactive
startup sweep across every session on disk — a durable parent→children
index does not exist today, and `ListSessions`' cheap header decode
(`readSessionInfo`) does not currently read `TaskParentID` or the last
record's type — a larger, separate piece of work, deliberately deferred
rather than folded into this fix.

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
- Agent-definition loading: built-ins, `.agents/*.md` parsing, unknown
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
