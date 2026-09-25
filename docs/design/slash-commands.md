# Slash commands: one registry, two kinds

## Motivation

Harness has no command vocabulary. A frontend that wants to compact a
session, change the model, or set an effort level must know the HTTP route
and its body shape. Harness ships no web UI, so every such frontend lives
outside this repository: the console, the TypeScript SDK, an ACP client.
Each one repeats that knowledge.

The console is the clearest case. Its command palette
(`web/src/components/command-palette/`) already holds a vocabulary for BOX
and APP actions: archive, hibernate, cancel a turn, new session. It holds
no vocabulary for a session operation, and `/compact` reaches nothing
there today.

A user also has no way to name a reusable prompt. Agent Skills
(`skill/`) cover the model-invoked case: the model reads a description and
decides to load the body. Nothing covers the human-invoked case, where a
person types a short name and gets a long, project-specific prompt.

This document specifies slash commands for harness. A slash command is a
name a human types. It resolves to one of three things: a session
control operation, an expanded prompt, or a frontend action.

Scope is harness alone. A session in `harness run`, `harness serve`, and
the SDK gets the complete command set with no other service present.
Section 10 records why.

Harness renders no menu. It owns the vocabulary, resolves a typed command on
`harness run`'s own terminal, and resolves one again inside `harness serve`'s
own prompt routes for a caller that declares its input typed (see
"Serve-mode resolution" below). Every graphical menu is a client's, and
`GET /commands` is what a client builds it from.

## 1. Three kinds, one registry

| Kind | Example | Effect | Source |
|---|---|---|---|
| Control | `/compact`, `/model` | Runs one existing session operation | Compiled-in table |
| Frontend | `/new`, `/clear` | Changes which session the user talks to | Compiled-in table |
| Prompt | `/review HEAD~1` | Appends one user message | `.agents/commands/*.md` |

All three share one registry, one lookup, and one completion shape. A
frontend renders one menu and does not care which kind a name is until it
dispatches.

A `KindFrontend` entry carries no `Op` and no route. It exists so `/help`
and completion stay complete, and so a frontend that cannot perform it
says so instead of ignoring the keystroke. Section 4 says which names
these are.

Skills stay model-invoked. Commands stay human-invoked. The model gets no
tool that calls a command.

## 2. The registry

`command.Spec` describes one command. The shape follows fx's
`SlashSpec`, reduced to the fields harness needs:

```go
type Spec struct {
    Name        string
    Aliases     []string
    Kind        Kind     // KindControl, KindFrontend, or KindPrompt
    Op          Op       // control only; empty for every other kind
    Summary     string   // one line, for the menu
    ArgHint     string   // "<ref>", shown after the name
    Args        []ArgSpec
    Category    Category
    Destructive bool     // a frontend must confirm before it fires
    Source      string   // "builtin", or the absolute file path
}
```

`ArgSpec` names one positional argument, its type, and whether it is
optional. Its name is the key a dispatcher reads. `/compact 5` produces
`{"keep_turns": 5}`, which `server` sends as a JSON body and `cmd/harness`
reads into `engine.CompactOptions`.

The registry sorts by name. `Aliases` resolve to the same `Spec`. A
builtin name is reserved: a file command may not shadow one.

`Destructive` marks a command that loses context or a turn: `/compact`,
`/abort`, `/clear`. Section 3 gives the parse rules that protect it.

## 3. Resolve is pure

```go
func (r *Registry) Resolve(line string) (Resolution, error)
```

`Resolve` parses one input line and returns what to do. It performs no
I/O, calls no route, and imports neither `engine` nor `server`.

```go
type Resolution struct {
    Kind Kind
    Spec *Spec
    Op   Op             // control: OpCompact, OpSetModel, …
    Args map[string]any // control: {"keep_turns": 5}
    Text string         // prompt: the expanded message
}
```

A control resolution names an operation and its arguments. It does NOT
name a route. The `command` package holds no transport and no session, so
`cmd/harness`, `server`, and any HTTP client share one parse.

Route mapping belongs to `server`, which owns the routes. Section 5
explains why an abstract `Op` is the right currency.

A rest argument keeps its internal spacing and drops its trailing
whitespace. The leading whitespace is the separator, and trailing
whitespace is invisible: two goal conditions that differ only by trailing
spaces are the same condition, and keeping the difference would make them
look different in a log.

A line that does not start with `/` is not a command. `Resolve` reports
that, and the caller sends the line unchanged. For `harness run`, an
unknown `/name` is an error: a frontend must not send it to the model as
literal text, with one exception below. `harness serve` differs: a typed
unknown `/name` stays a prompt, sent on unchanged, for every session —
see "Serve-mode resolution"'s "Resolving the line" section.

One case sends an unknown `/name` on as literal text, on purpose: a
session delegated to the Claude Code CLI (`engine.Session.ClaudeCodeDelegated`).
That CLI is not "the model". It is a second frontend with its own slash
vocabulary — `/cost`, `/context`, `/usage`, and more, which it advertises
in the `slash_commands` array of its own `init` line. `harness run` owns
no route for a name like `/cost`; the delegated CLI does. So `harness run`
sends the unresolved line to `Session.Prompt` unchanged, and the CLI
answers with its own output, or reports its own unknown-command error for
a name it does not know either. `command.Registry.Resolve` itself stays
unchanged: it still returns `UnknownCommandError` for every unknown name,
delegated session or not. Only the caller's response to that error
differs, and only on a delegated session.

### Parsing is strict

A slash parser sits between a human and a destructive operation. Claude
Code shows the failure mode. This line:

```
/clear is just an alias for new
```

matched `/clear`, passed `is just an alias for new` as arguments, ran the
command, and discarded the sentence. The user wrote prose ABOUT a command
and lost the context instead.

Four rules prevent this. They live in `Resolve`, so every frontend gets
the same answer.

1. **Surplus input is an error.** A command with no `Args` rejects
   trailing text. `Resolve` never drops part of a line.
2. **A command is the whole input.** One line, nothing after it. A
   multi-line message is not a command.
3. **`//name` is literal.** It resolves to the text `/name`. A user must
   be able to write about a command.
4. **Ambiguity takes the safe reading.** A line that parses only after
   `Resolve` drops text is not a command.

Rule 1 alone prevents the failure above.

A menu is autocomplete, not a fifth protection. It filters candidates as
the person types and fills the input on selection; it does not run the
command itself. The command runs only once the line is submitted, and the
server resolves it there, under the same four rules every time — a menu
selection carries no special trust of its own. `harness run -p
"/compact"` has no menu at all, so rules 1 through 4 carry the full
weight there too.

## 4. Control commands

Every v1 control command names an `Op`. No new session behavior enters
with this design.

The table below is the SERVE-mode mapping, owned by `server`. Every route
in it exists now (`server/server.go`). `cmd/harness` maps the same `Op`
set to method calls instead — see section 5.

| Command | Method and path (serve) | Args |
|---|---|---|
| `/compact` | `POST /session/{id}/compact` | `keep_turns` |
| `/model` | `POST /session/{id}/model` | `model` |
| `/thinking` | `POST /session/{id}/thinking` | `effort` |
| `/tier` | `POST /session/{id}/service-tier` | `service_tier` |
| `/abort` | `POST /session/{id}/abort` | — |
| `/goal` | `POST /session/{id}/goal` | condition |
| `/goal-clear` | `DELETE /session/{id}/goal` | — |
| `/queue` | `GET /session/{id}/queue` | — |
| `/queue-clear` | `DELETE /session/{id}/queue` | — |
| `/status` | `GET /session/{id}` | — |
| `/processes` | `GET /process` | — |

`/model` completes from `modelmeta`. The catalog is static, so completion
costs no network call.

This table matches the shipped registry (`command/registry.go`), which
differs from the table an earlier draft of this design carried, in four
ways:

- `/goal clear` and `/queue clear` shipped as `/goal-clear` and
  `/queue-clear`. A space-separated subcommand needs a second token in the
  name-and-rest split that section 3 does not otherwise require. A hyphenated
  name resolves through the same single-token lookup as every other command.
- `/compact` takes `keep_turns` only. A one-shot `model` override on a
  single compact call adds a second way to change the session's model,
  besides `/model` itself. `/model` stays the one place a model change
  happens.
- `/help` did not ship. `GET /commands` already serves the same registry a
  `/help` command would read, so a frontend can build its own help surface
  from that response. Whether a frontend ALSO wants a `/help` command that
  renders it inline stays open — see Deferred decisions.
- `/mcp` did not ship. An earlier draft of this table paired the name with
  `POST /session/{id}/mcp`, reading that route as "reload MCP servers". That
  route is `handleSessionMCP`, the Streamable HTTP MCP JSON-RPC endpoint —
  not a reload. No reload operation exists anywhere in the engine to name
  instead, so the row was removed rather than repointed. Do not re-add
  `/mcp` on the strength of that path alone.

These fx commands stay out: `/login`, `/logout`, `/credits`,
`/permissions`, `/allowlist`, `/undo`, `/statusline`, `/notifications`,
`/feedback`. A permission system is a settled non-goal. The others are
frontend chrome or a hosted-product concern.

`/new`, `/resume`, and `/quit` name no session operation. They choose
WHICH session a frontend talks to. `/clear` is an alias of `/new`, not a
separate operation. Harness never erases a log, so "clear" means "point at
a fresh session". The old log stays on disk, which makes the command
reversible: point back.

The owner of that pointer differs per frontend. The console keeps it in
`boxes.current_session_id`, and its command palette already exposes the
action. `harness run` keeps it in a local variable.

So v1 lists these names as `KindFrontend`, with `Destructive` set on
`/new` and `/clear`. They carry no `Op` and no route, and each frontend
dispatches its own. `GET /commands` returns them with no `method` and no
`path`, which is how a client knows it owns the action.

## 5. Dispatch

The engine does not interpret a control command. `Session.Prompt`
(`engine/engine.go`) keeps its signature and learns no control verb; the
command string stops at the frontend. A caller that skips `Resolve` and
sends `/compact` as a prompt to a NATIVE session gets an ordinary prompt:
the model sees the text `/compact`, and nothing compacts. On a
claude-code-delegated session, the same text still reaches the CLI
unchanged and the CLI runs it as its own command (see the CLI-vocabulary
callout below); the appended message keeps the caller's own origin, not
`OriginEngine`. `Session.RunCompactCommand` (`engine/compact.go`) is the
one engine entry point for a RESOLVED compact command, reached only
through `POST /session/{id}/compact` (serve mode) or the `run`-mode
dispatcher, never through `Session.Prompt`. It calls `Session.Compact` on
a native session. A claude-code-delegated session has no journal to fold,
so `RunCompactCommand` issues the CLI's own compact command instead, with
`OriginEngine`.

`harness serve` resolves a typed command earlier still, before
`Session.Prompt` is ever called: `resolvePromptCommand`
(`server/commands.go`) runs first, on every prompt-landing route, for
every op serve mode supports — not `/compact` alone. See "Serve-mode
resolution" below for the full rule.

> **One exception to the `//x` escape.** Rule 3 promises literal text
> for `//name`. One path still acts on that text anyway.
>
> **A delegated session's own CLI vocabulary, permanently.** The
> Claude Code CLI reads a leading `/name` in the text it receives as
> one of ITS OWN slash commands (`/cost`, `/context`, `/usage`, and
> more — section 2). A typed `//x` on a session delegated to that CLI
> (`Session.ClaudeCodeDelegated`) still unescapes to `/x`, and that
> text still reaches the CLI as this turn's own input
> (`dispatchClaudeCodeTurn`), in `harness run` and `harness serve`
> alike. Harness cannot keep it literal there: the CLI owns that
> name, not harness. A typed `//compact` on a NATIVE session, by
> contrast, unescapes to the plain text `/compact` (rule 3) and reaches
> the model as ordinary input: the engine learns no control verb from
> prompt text, so nothing compacts.

Three reasons, in order of weight.

**Session resolution is server state.** The engine holds no id-to-session
registry. `handleSetModel` (`server/handlers.go`) spends about 35 lines
choosing WHICH `*engine.Session` to mutate: a managed child comes from
`s.sessMgr`'s own resident node, a root goes through the `s.sessions`
residency map with a cold load, an insert race, and eviction. Its comment
states the invariant — "two `*engine.Session` for one log must never both
be mutated", because `SetModel` persists a durable `recModel` record. A
control command carries a session id, not a session. Turning that id into
the one correct object is the server's job, and nothing below it can do
the work.

**Some rejections are pure topology.** `rejectManagedChildTurn`
(`server/handlers.go`) refuses a session whose `TaskParentID` is set and
names `POST /session/{id}/send` instead (see
`design/session-send-unification.md`). That is routing, not an engine
rule. The engine has no opinion about it and should not grow one.

**The concurrency shape differs per verb.** `SetModel` does not take the
run slot — it applies while a turn runs. `Compact` goes through
`claimForPrompt`, which also handles draining, residency, and a `503`.
Each route already encodes its own answer. One engine-side entry point for
"run a control verb" would have to re-derive that per verb, and every
existing guard would need a re-audit against the new path.

Defense in depth already exists and is not the argument here.
`Session.Compact` carries its own Claude Code delegation guard, and
`engine/compact.go` names it the authoritative one. `RunCompactCommand`
routes a delegated session around that guard, to the CLI's own compact
command. The engine is not missing guards. It is missing the session
registry and the routing rules.

### Two dispatchers, one registry

Every reason above is a reason about `harness serve`. `harness run` builds
no `server.Server`. It constructs an `engine.SessionManager` and holds one
root session as a local variable (`cmd/harness/main.go`). So harness has
two composition points, and each maps `Op` its own way.

| | `harness run` | `harness serve` |
|---|---|---|
| Session resolution | One session, already in hand | Residency map, cold load, eviction |
| Managed-child rejection | Not reachable: the root is adopted, never a child | `rejectManagedChildTurn` |
| Run slot | No contention to arbitrate | `claimForPrompt` |
| `OpCompact` maps to | `Session.RunCompactCommand` (native: `Session.Compact`) | `POST /session/{id}/compact` -> `Session.RunCompactCommand` |

Both dispatchers call `Session.RunCompactCommand`. It calls `Session.Compact`
on a native session, and issues the Claude Code CLI's own compact command
on a delegated one. A direct call from `cmd/harness` is safe because the
two server-only concerns above do not exist in run mode — there is one
session, and it is a root — and because `RunCompactCommand` handles the
Claude Code lane itself: a caller needs no separate check before it calls
the function.

Not every mutator guards itself this way. `Session.SetModel` has no
internal guard at all; it persists unconditionally. `Session.ModelSupported`
and `Session.CheckModel` are checks the CALLER must run before the swap,
not guards inside the setter — `handleSetModel` (`server/handlers.go`) runs
both before it calls `SetModel`, and a run-mode dispatcher must run the
same two checks before its own call, in the same order, or it durably
persists a swap the server would have rejected. Read each setter's own
guarantee before calling it directly; do not assume the engine guards
every mutation for you.

This is why `Resolution` names an `Op` and not a route. A route is one
dispatcher's answer, not the operation.

The two dispatchers do NOT cover the same `Op` set. Run mode maps an `Op`
to a method the engine already exports: `Session.RunCompactCommand`,
`Session.SetModel`, `Session.SetEffort`, `Session.SetServiceTier`.
`/abort`, `/goal`, and `/queue` have no such shape there — abort arbitrates
a run slot, `Session.PursueGoal` is a long call and not a setter, and the
queue is server state. So each `Op` declares which dispatchers support it,
and an unsupported `Op` reports "not available in this mode" with the
reason. Silence is the failure to avoid.

- An HTTP client (the console, the SDK) reads `method` and `path` from
  `GET /commands` and makes the request.
- `cmd/harness` maps `Op` to a method call on the session it holds.
  `harness run -continue -p "/compact"` compacts the continued session with
  no server present.
- `harness run` with neither `-resume` nor `-continue` has no prior history. A
  control command there reports a clear error.

Every error a route returns today becomes the command's error in serve
mode. A `409` from a busy session reaches the user with its existing
text. In run mode the engine's own error text surfaces instead.

## 6. `GET /commands`

The server returns the resolved registry for the current configuration:

```json
{"commands": [
  {"name": "compact", "kind": "control", "op": "compact",
   "summary": "…", "arg_hint": "[keep_turns]", "category": "session",
   "aliases": [], "args": [{"name": "keep_turns", "type": "int",
   "optional": true}],
   "method": "POST", "path": "/session/{id}/compact"}
]}
```

A frontend builds its slash-menu autocomplete from this response, rather
than carrying its own table. The menu only fills the input; harness
resolves the typed line server-side, so a frontend never re-implements
`Resolve`. `method` and `path` are the serve-mode mapping, present so an
HTTP client needs no route knowledge of its own; `cmd/harness` ignores
both and dispatches on `op`.

`serve_support` is a second, additive map, keyed by the same canonical
`name`, for a client that resolves commands itself — the Boxes console,
notably; harness has no console of its own — rather than proxying through
a frontend that owns the operation. Each entry reads
`{"supported": true}` or
`{"supported": false, "reason": "Not available in this client."}`. A
frontend command (`new` — aliased `/clear` —, `resume`, and `quit`) is
unsupported because the frontend owns the session pointer, not serve
mode; `queue-clear` is unsupported because `DELETE /session/{id}/queue`
already exists but nothing dispatches it through serve mode yet. The
reason string names no route and no internal term — it is exactly what
serve mode returns to the person who typed the command. A response
carrying no `serve_support` key at all means serve mode resolves nothing
— an older server, notably.

Add the entry to `server/openapi.yaml`, which stays authoritative.

### The menu lives in the input area

The slash menu is a composer affordance. A `/` at the start of the input
opens it, and typing filters it. It is NOT the console command palette.

The two stay separate on purpose. The palette acts on the box and the
app: archive, hibernate, new session. The slash menu acts on the CURRENT
conversation: compact it, change its model, set its effort. One mixed
list makes both harder to scan, and the two surfaces answer different
questions.

Separate surfaces do not mean separate vocabularies. The menu builds from
`GET /commands`. Where a name exists in both surfaces, the slash menu
calls the same action the owning component exposes, exactly as the
palette bridge does today. Neither surface reimplements a guard.

The menu fills the input on selection. It does not fire the command;
submitting the line does. Section 3 gives the reason.

## Serve-mode resolution

`harness serve` resolves a slash command on submit, inside its own
prompt-landing routes: `POST /session/{id}/prompt_async`,
`POST /session/{id}/enqueue`, and `POST /session/{id}/send`. All three
call one shared entry point, `resolvePromptCommand`
(`server/commands.go`), before any other branch runs. A resolved command
never appends a user message, and it never enters the prompt queue (see
`docs/session-storage-and-queue.md`).

### The typed gate

Resolution runs only when the request's declared provenance
(`message.PromptSource`, normalized) equals `typed`. Normalizing maps an
empty or omitted source to `api`, so an unlabeled caller stays a plain
prompt — a typed line must be asserted, never assumed.

The gate filters on a source the CALLER DECLARES. It is not a security
boundary. Harness authenticates every caller with one bearer token, not
one trust level per human or service, so anything holding that token —
a delegated Claude Code CLI process inside the same box included — can
declare `typed` for its own request. The gate exists only to stop an
untagged programmatic caller's literal `/foo` text from being silently
reinterpreted as a control command. It proves nothing about who actually
typed the line.

### Resolving the line

A typed line goes to `command.Registry.Resolve`. Four outcomes follow.

- **Not a command** (`command.ErrNotCommand`): no record is journaled;
  the line is sent on as an ordinary prompt. `//x` becomes the literal
  text `/x` here, and only here — a non-typed `//x` is never resolved,
  and reaches the model, or the queue, exactly as typed. Two cases still
  act on that unresolved text downstream: see the "Two exceptions"
  callout in section 5.
- **Unknown name** (`command.UnknownCommandError`): no record is
  journaled; the line stays a prompt, sent on unchanged, exactly like an
  unresolved non-typed line.
- **Bad arguments** (`command.ArgsError`): a `failed` record is
  journaled at once, with `Resolve`'s own error text. Nothing runs.
- **A known command**: the checks below run first, in order, and settle
  the one record this branch journals:
  1. The request carries an attachment: `failed`, and nothing runs.
  2. `serve_support` reports the command unsupported: `unsupported`,
     and nothing runs.
  3. A turn is already running, and the command does not declare
     `available_during_task`: `refused`, and nothing runs. The command
     is refused outright, never queued for later.
  4. Otherwise: `accepted`, then the command's own route runs in
     process — the exact handler its own HTTP method would call — and
     a second, terminal record follows.

`<name>` in every rendered sentence below is the name (or alias) the
caller actually typed, never the registry's canonical `name`.

### Status and text

| Status | Text |
|---|---|
| `accepted` | omitted |
| `succeeded` | `/<name> succeeded` |
| `failed` (bad arguments) | `Resolve`'s own error text, e.g. `command: /compact keep_turns must be a number, got "abc"` |
| `failed` (attachment) | `/<name> takes no attachments; nothing ran` |
| `failed` (route error) | the route's own `error` string |
| `failed` (compact skip) | `/compact did nothing: <reason>` (`engine.CompactSkipMessage`) |
| `refused` | `/<name> cannot run while a turn is running; send it again after the turn ends` |
| `unsupported` | `/<name> is not available in this client` |
| `interrupted` (boot) | `harness restarted before /<name> finished; it will not run again` |
| `interrupted` (drain) | `harness stopped before /<name> finished; it will not run again` |

Boot and drain `interrupted` come from two different places. At boot, a
command still `accepted` never reached a terminal status before the
process restarted — its dispatch will not resume, so
`Session.RepairInterruptedCommands` rewrites it to `interrupted` before
any client can observe it stuck. Mid-run, a dispatched command's own
route call can fail because the process is already draining for
shutdown; that failure maps to `interrupted` too, rather than `failed`,
so a client can tell "the process gave up on this" from "the command
itself was wrong".

### The record

Every resolution that reaches a `CommandRecord` at all journals
`message.CommandRecord`, in the same shape whichever route resolved it:
`id`, `line`, `name`, `source`, `status`, `created_at`, and `updated_at`
are always present; `args`, `source_id`, `source_label`, `text`, `result`,
`result_truncated`, and `after_message_id` are present only when non-empty.
`result` is the dispatched route's own 2xx
JSON body, capped at 16 KiB; over the cap, `result` is omitted and
`result_truncated` is `true` instead. `after_message_id` anchors the
command to the last durable message at the moment of its first record —
empty means "before every message" — and a later compaction re-anchors
it to the fold's own summary id, so the anchor survives however many
compactions later. A record anchors to a message by its ID. The
transcript already treats message IDs as unique: clients de-duplicate
pages by ID. If a caller reuses a message ID, a command record can
render after either copy of that message, and it can appear on the
page of each copy. Clients upsert records by `id`, so a record that
appears twice is harmless.

A dispatched command produces exactly two records sharing one `id`:
`accepted`, then one terminal status. Bad arguments, an attachment,
unsupported, and refused each produce exactly one record. Not a command
and unknown name produce none — see "Resolving the line" above.
`POST /session/{id}/enqueue`'s own idempotency
`seq` covers a command exactly like an ordinary prompt: a duplicate
`seq` answers a clean `{"status": "duplicate", "watermark": N}`, and
nothing runs a second time.

### The carriers

A resolved command reaches a client three ways:

- The durable `"command"` SSE event —
  `{"type":"command","session_id":"…","seq":…,"recorded_at":"…","command":{…}}`
  — replayable from any `from=<seq>` like every other durable event.
- `commands` on `GET /session/{id}/message`'s bootstrap (`stream_from`)
  and page (`before_seq`/`limit`) responses: the folded records whose
  `after_message_id` anchor falls inside the returned window. When that
  window starts at the session's first message, this also holds every
  record with an empty `after_message_id`. Always present, `[]` when
  empty.
- `GET /session/{id}/journal`, as metadata beside its own record:
  `command_id`, `command_name`, and `command_status` — never the full
  record, matching every other journal field's sanitized-metadata rule.

## 7. Prompt commands

Stage 2. A prompt command is a Markdown file under `.agents/commands/`,
beside the existing `.agents/skills/` and `.agents/*.md` agent
definitions (`engine/agentdef.go`).

```
.agents/commands/review.md      -> /review
.agents/commands/git/sync.md    -> /git:sync
```

```markdown
---
description: Review a diff for correctness bugs
argument-hint: <ref>
---
Review the changes in $1 against AGENTS.md. Report only defects.
```

The parser reuses `skill/frontmatter.go`. Extract `splitFrontmatter` and
`parseFrontmatter` into a shared internal package, or export them. Do not
add a YAML dependency and do not add a second parser.

Substitution covers `$ARGUMENTS` and `$1` through `$9`. Nothing else
expands. Section 10 records why command files run no shell.

Config gains `commands_dirs`. It layers exactly like `skills_dirs` and
`agent_defs_dirs`: a nil list keeps the project default, an explicit empty
list disables discovery, and a project value replaces a user value.

Duplicate names resolve by precedence, not by failure: a project command
shadows a user command of the same name, and the loader logs the shadow.
This differs from skill discovery, which rejects a duplicate loudly. The
divergence is deliberate. A user's global `/review` must stay shadowable,
and a name collision must not fail a session.

## 8. Provenance and the log

A prompt command appends one ordinary user message. The message holds the
EXPANDED text, because that is what the model reads, and the log stores
canonical messages.

Attribution uses the path that exists: add `PromptSourceCommand` to
`message.PromptSource` (`message/message.go`) and stamp it through
`Session.PromptWithOriginFrom` (`engine/engine.go`), which already carries
a `PromptProvenance` onto the appended message. The typed line
(`/review HEAD~1`) rides as provenance, so a frontend renders what the
human typed.

No new event type enters. Expanded command text is user-trust text. It
never becomes a `message.EngineContext` part.

## 9. Startup budget

Command discovery reads no disk before first output.

- A named lookup stats one path.
- A full directory scan happens only when a caller asks for the menu
  (`GET /commands`, `/help`).
- Stage one reads frontmatter only, matching the skill contract. The body
  loads when the command runs.

## 10. Rejected alternatives

**The engine interprets control commands.** Rejected. The engine holds no
id-to-session registry, and resolving one id to the single mutable
`*engine.Session` for a log is server work. See section 5.

**Each frontend parses its own commands.** Rejected. The console, the SDK,
and an ACP client would each re-derive that `/compact 5` means
`{"keep_turns": 5}`.

**Shell substitution in a command file.** Rejected for v1. Claude Code and
fx both expand a backtick-quoted command inside a command body. Harness has
no permission system, and adding one is a settled non-goal. Expansion at
TYPE time would run a checked-in repository file's shell before any model
turn. The bash tool already runs unsandboxed, so this adds no capability,
but it adds a new trigger. Reintroduce it only as an explicit decision.

**A tool that lets the model call a command.** Rejected. Skills already
cover model-invoked instructions. A second mechanism splits that story.

**The control plane owns control commands.** Rejected. A harness session
must work with no other service present. `meetneptune/boxes` has no
compaction endpoint at all, so `/compact` would exist nowhere. That repo
adds fleet commands (`/spawn`, `/hibernate`) and wraps some harness routes
for its own reasons — a session-less box, error remapping, a hibernated
box. Wrapping is not owning, and it is out of scope here.

## 11. Staging

1. `command` package: `Spec`, `Registry`, `Resolve`, `Op`, the builtin
   control table. No I/O, no routes.
2. `server` dispatcher: the `Op`-to-route map, `GET /commands` (including
   `serve_support`), and the `server/openapi.yaml` entry.
   `resolvePromptCommand` resolves a TYPED line on every prompt-landing
   route (`prompt_async`, `enqueue`, `send`) and journals its
   `CommandRecord` — see "Serve-mode resolution" above.
3. `cmd/harness` dispatcher: `Op` to a method call on the held session,
   for `-resume` and `-continue` runs.
4. Console menu: `/` in the input area opens a menu built from
   `GET /commands`. This stage lands in `meetneptune/boxes`, not here.
5. `.agents/commands/*.md` discovery, `commands_dirs`, substitution, and
   `PromptSourceCommand`.

Stages 1 through 4 add no new session behavior. Stage 5 adds the first new
file format. Stage 3 is optional for a first release: it adds no
capability `harness serve` lacks, but it proves the `Op` currency carries
two dispatchers.

## 12. Testing

Name the failure first.

- `Resolve("/compact 5")` returns `{"keep_turns": 5}`, not `{"keep_turns":
  "5"}`. Assert the type.
- `Resolve("/compact abc")` is an error, not a request with a zero body.
- `Resolve("review this")` is not a command.
- `Resolve("/nope")` is an error. Assert that no prompt is sent.
- `Resolve("/clear is just an alias for new")` is an error. Red-verify it
  against rule 1: no command runs, and no text is dropped.
- `Resolve("//clear")` returns the literal text `/clear`, not a command.
- A two-line input whose first line is `/compact` is not a command.
- An alias resolves to the same `Spec` pointer as its name.
- A file command may not shadow a builtin name.
- `GET /commands` lists every builtin, sorted, with no surplus entry.
- Every `KindControl` entry carries an `Op`. No `KindFrontend` entry
  carries one, a `method`, or a `path`. Assert both directions.
- A `/compact` against a managed child still returns the existing `409`.
  Red-verify against `rejectManagedChildTurn`.
- Both dispatchers accept the same `Resolution`. Assert that each
  dispatcher handles every `Op` that declares support for it, and reports
  an explicit error for every `Op` that does not. A silently unhandled
  verb must fail the test.
- Stage 4: a project command shadows a user command of the same name.
- Stage 4: an appended message carries `PromptSourceCommand` and the
  expanded text.
- A `prompt_async`/`enqueue`/`send` request with an empty or `api`
  source and text `/compact` stays a plain prompt through
  `resolvePromptCommand`; only `source: "typed"` resolves it.
- A typed `//x` resolves to `handled=false` and the literal text `/x`,
  never a `CommandRecord` (`server/command_resolve_test.go`'s `//model x`
  case). A non-typed `//x` reaches the model, or the queue, unchanged.
  Whether that unescaped text then acts as a command downstream is the
  separate "One exception" callout in section 5, not this rule.
- A typed `/queue-clear` records `unsupported` with
  `"Not available in this client."` and the queue is untouched.
  Red-verify against `serveModeOps`.
- A typed dispatchable command against a busy session records
  `refused`, not `accepted`, and nothing is queued.
- A dispatched command journals exactly two records sharing one `id`:
  `accepted`, then one terminal status. Assert no third record for the
  same `id`.
- A command still `accepted` at boot is repaired to `interrupted` before
  any client can observe it. Red-verify against
  `RepairInterruptedCommands`.

Table tests suit `Resolve`. Use `httptest` for `GET /commands`.

## Deferred decisions

None of these blocks stage 1. Each is deferred until the stage that forces
it.

| Question | Forced at |
|---|---|
| Does `/status` return the session JSON verbatim, or a reduced shape a menu can render? | Stage 2, when the route map ships |
| Does `/help` belong in the registry, or is it a frontend concern like `/new`? | Stage 4, when a menu renders it |
| Should `Resolve` accept a trailing `--json` for a scripted caller? | Not forced. Add it only for a named caller |
