# Lazy MCP tools: deferred schemas, search, and select

## Motivation

Every configured MCP server contributes its whole tool catalog to every
request. `Session.toolDefs` (`engine/engine.go`) appends
`s.cfg.MCP.Tools(ctx)` verbatim, and `MCPManager.rebuildToolsLocked`
(`engine/mcp.go`) returns one `provider.ToolDef` — name, description, and
full JSON Schema — for every tool of every connected server. A box that
wires a browser server, an issue tracker, a database, and an observability
gateway therefore ships hundreds of schemas in the tools array of every
turn, before the model has read one word of the user's request.

The cost is structural, not incidental:

- Tool schemas sit at the FRONT of the cached prefix on every provider
  (Anthropic caches tools, then system, then messages — see AGENTS.md,
  "The tool array is byte-stable across requests"). A large catalog
  inflates every cache write and every cache read for the life of the
  session.
- A catalog the model never uses still competes for attention with the
  tools it does use. Large tool sets measurably degrade tool selection.
- The connection itself is already lazy (`MCPManager.ensureConnected`), so
  the box pays no dial cost for an unused server, but it pays the full
  schema cost the moment that server connects.

This document specifies deferred loading: an MCP tool registers as a
NAME-ONLY catalog entry, and the model loads the schema it needs with a
`select` call before it uses the tool. It follows the same
progressive-disclosure model the engine already uses for Agent Skills
(`engine/skills.go`): stage 1 is name plus one-line description, stage 2 is
an explicit fetch.

The behaviour is opt-in. A config that does not ask for it keeps today's
eager registration, byte for byte.

## 1. What the model sees

With deferral active for a server, one request carries:

1. A system segment (stage 1) that lists every deferred tool as
   `name — one-line description`, plus a header that states the contract:
   the model MUST call `mcp(action="select", ...)` before it uses a
   deferred tool.
2. A tools array that holds the built-in tools, the SELECTED MCP tools
   (full schemas), and plugin tools — in that group order, unchanged.

The model calls `mcp(action="search", query="...")` to find a tool by
keyword, then `mcp(action="select", tools=[...])` to load it. The next
provider request in the SAME turn carries the selected tool's full schema
in the tools array, because `runAgenticLoop` (`engine/engine.go`) rebuilds
the request — and therefore calls `toolDefs` again — on every tool round.
The model then calls the tool directly, exactly like a statically
registered one. No proxy call shape, no argument re-encoding.

## 2. Config

Global policy, in `config.Config` (`config/config.go`):

```json
{
  "mcp_tool_loading": "auto",
  "mcp_tool_loading_threshold": 20
}
```

`mcp_tool_loading` is a three-value enum, validated like `session_sync`:

| Value | Meaning |
|---|---|
| `"eager"` | Today's behaviour. Every MCP tool registers with its schema. The default when the key is absent. |
| `"auto"` | Defer when the live catalog holds more tools than the threshold. |
| `"lazy"` | Always defer, whatever the catalog size. |

`mcp_tool_loading_threshold` is the tool COUNT `"auto"` compares against.
`0` or absent means the engine default (`defaultMCPDeferThreshold`, 20).

`Config.validate` rejects a NEGATIVE value, exactly as `connect_timeout_s`
is rejected today. The reason is that `len(catalog) > -1` holds even for an
empty catalog. A stray minus sign would silently turn `"auto"` into
"always defer". The engine resolves any non-positive value it is handed to
that same default of 20. An embedder that bypasses config validation
therefore stays safe. The engine never clamps to a floor of 1: that value
would BE the always-defer bug.
A count, not a token estimate. The engine has no tokenizer on the request
path, and a count is deterministic and testable. Twenty is the point where
a typical catalog's schema block stops being small enough to ignore. A
listing of twenty names costs a fraction of twenty schemas. The value is a
default, not a law: a box with unusually large schemas lowers it.

Per-server override, in `config.MCPServerSpec`:

```json
{
  "mcp_servers": {
    "github":   { "url": "...", "tool_loading": "lazy" },
    "internal": { "command": ["..."], "tool_loading": "eager" }
  }
}
```

`tool_loading` accepts `"eager"` or `"lazy"` only. It does NOT accept
`"auto"`: the threshold measures whole-catalog context pressure, which is
not a property of one server. An absent value inherits the global mode.

`validateMCPServers` (`config/config.go`) rejects an unknown value, the
same "a typo must not silently fall back" rule the file already applies to
`session_sync` and to `mcp_servers` entries.

### Where the policy lives in the engine

The policy travels on `engine.Config`, not on `engine.MCPServerConfig`:

```go
// MCPToolLoading selects when this session defers MCP tool schemas ...
MCPToolLoading MCPToolLoading
// MCPToolLoadingThreshold is the tool count "auto" compares against ...
MCPToolLoadingThreshold int
// MCPToolLoadingByServer overrides MCPToolLoading for one named server ...
MCPToolLoadingByServer map[string]MCPToolLoading
```

`*MCPManager` is a box-scoped singleton shared by every session (see
`engine/mcp.go`'s package doc). Deferral is per-SESSION state: two sessions
on one box select different tools from the same catalog. Connection
settings therefore stay on `MCPServerConfig`, and presentation policy sits
beside the session that applies it. `cmd/harness/mcp.go` translates
`mcp_servers.<name>.tool_loading` into `MCPToolLoadingByServer` when it
builds the manager, so the config author still writes the override next to
the server it names.

No method joins the `MCPRegistry` interface. The session derives everything
it needs from the `[]provider.ToolDef` slice `Tools(ctx)` already returns:
the name carries the server (`mcp__<server>__<tool>`, and
`validateMCPServers` guarantees a server name holds no `__`, so the split
is unambiguous), and `Description` supplies the listing text. Growing
`MCPRegistry` would force every out-of-package fake — `server`,
`cmd/harness` — to add a method for no gain, the same reasoning
`mcpStatusReader` and `mcpConfigReader` (`engine/mcp_status.go`,
`engine/mcp_tool.go`) already record.

## 3. Deferral core

### Effective mode

`resolveMCPLoading` decides per server, per request:

1. `MCPToolLoadingByServer[server]`, when set, selects the mode. It wins
   over the global mode absolutely.
2. Otherwise the global mode applies.
3. The selected mode decides: `eager` registers, `lazy` defers, and `auto`
   defers when `len(catalog) > threshold`, where `catalog` is every tool
   `Tools(ctx)` returned on THIS request, across every connected server.

Rule 1 selects a mode; it does not by itself decide eager-or-defer. The
distinction matters because `MCPToolLoadingByServer` is typed
`map[string]MCPToolLoading` and therefore ADMITS `auto` per server.
`config.validateMCPServers` rejects a per-server `auto`, so no config file
can produce one, but an embedder setting `engine.Config` directly can.
`resolveMCPLoading` handles it the only sane way: a per-server `auto` is
`auto`, resolved against the same whole-catalog threshold as the global
mode. It is not an error, and it is not silently read as `eager`.

Rule 3 counts the WHOLE catalog, including the tools of a server pinned
`eager` by rule 1. A pinned server's schemas fill the prompt like any
other, so they are part of the pressure the threshold measures. Pinning a
large server `eager` therefore makes `auto` defer the rest sooner, which is
the intended reading: the pin says "always keep these loaded", not "ignore
their cost".

One condition overrides all three: a session that does not hold the `mcp`
tool defers NOTHING. `resolveMCPLoading` reports `eager` for every server
when `Session.tools` has no `mcp` entry.

Never defer what the session cannot select. The `mcp` tool is absent in two
cases. A session with no configured server has no catalog to defer, so the
rule is moot there. The load-bearing case is a subagent: `restrictTools`
(`engine/session_manager.go`) narrows a child to an agent definition's
`tools:` list, and a definition that omits `"mcp"` removes the tool.
Without this rule, deferral would strip that child's MCP schemas AND its
only path to load them back — a lockout, converting a restriction that is
harmless today into a total loss of MCP for that child. The child instead
keeps eager registration, which is exactly its behaviour before this
change.

Rule 3 reads live state, so the decision can flip mid-session: a second
server connects (a background retry commits, or the `mcp` tool's `connect`
action succeeds), the catalog crosses the threshold, and tools that were
registered eagerly on turn N are deferred on turn N+1. This is deliberate.
The alternative — latch the first decision — freezes a session on a
one-server catalog it no longer has.

### Use implies selection

A flip must not take away a tool the session is already using. A tool of an
eager server needs no `select` call — the design tells the model NOT to
select a loaded tool (§4) — so nothing would hold its schema across the
flip, and a working tool would silently lose its definition mid-task.

A routed MCP tool call adds its own name to the selected set, when that
tool's OWN SERVER could ever defer. `executeTool` (`engine/engine.go`)
already resolves the binding before it calls the server; a name that
resolves is a real tool by construction, so this can never record an
invented one. A tool the model has actually used stays loaded across a
flip, across a later `select` of something else, and across a reload.

The gate is per SERVER, not per session, and the difference is not
cosmetic. The record exists only to survive a flip to deferred, and a
server pinned `eager` by `MCPToolLoadingByServer` can NEVER flip — rule 1
is absolute — so a record for its tools could never pay for itself. A
session-level gate would still write one for every tool of that pinned
server, merely because some OTHER server in the same session is `lazy`.
The rule is therefore: record when `resolveMCPLoading`'s selected mode for
that tool's server is not `eager` (`auto` counts, since a catalog can cross
the threshold at any moment).

That also keeps the whole mechanism off the default path: under a plain
`eager` config no server can ever defer, so nothing is recorded at all,
and a config that opted into none of this spends no log bytes and no disk
write on its hot path.

This also closes the recovery path in §7 from the other end: a model that
calls a deferred tool without selecting it — replaying a name from earlier
history — gets the call served AND the schema loaded for the next round,
instead of working once by luck and then vanishing.

The record is written when a tool ENTERS the set, not once per session: a
repeat call finds the name already there and writes nothing, while a tool
that was reaped (§4) and then used again enters a second time and writes
again. Replay dedups, so the set is identical either way.

### The tools array

`toolDefs` partitions the MCP slice instead of appending it whole:

- A tool from an EAGER server is appended with its schema, exactly as
  today.
- A tool from a DEFERRED server is appended only when its name is in the
  session's selected set.

Order is unchanged. The filter preserves the incoming order, and
`rebuildToolsLocked` already sorts by server then tool, so the array stays
byte-stable across requests that change no selection — the property
`engine/tooldefs_order_test.go` guards.

### The catalog segment

`mcpCatalogSegment` renders stage 1. It is a SYSTEM segment, appended after
the Agent Skills segment and before hook (`system.transform`) segments —
the position the Skills catalog already occupies for the same reason.

The segment is computed fresh per request from the same
`[]provider.ToolDef` slice `toolDefs` used, never cached for the session:
the catalog changes when a server connects, and a stale listing would name
tools `select` cannot find.

That forces one ordering change in `streamTurn` (`engine/engine.go`). Today
`streamTurn` assembles the system slice first and computes
`tools := s.toolDefs(ctx)` afterwards. The catalog segment belongs INSIDE
the system slice, so the tool plan must exist before the slice is
assembled.

The new order is:

1. Apply the `chat.params` hook, which fixes `params.Model`.
2. Resolve the provider, `s.cfg.Providers.For(params.Model)`.
3. Compute the tool plan — ONE call returning the filtered defs and the
   catalog text together.
4. Assemble the system slice: base, instructions, skills, the catalog
   segment, then the `system.transform` hook segments.
5. Assemble the messages and their ambient blocks.

`Tools(ctx)` is still called exactly once per request, and the plan's defs
are still reused verbatim as `req.Tools`.

Step 2 stays AHEAD of step 3, and that placement is the whole reason the
plan does not simply move to the top of the function. `toolDefs ->
s.cfg.MCP.Tools(ctx)` is what TRIGGERS a server's first connect attempt:
network dials, and a spawned child process for every stdio server. Today
`Providers.For` runs first (`engine/engine.go`), so a turn naming an
unconfigured provider returns before any of that happens. A plan computed
at the top of the function would spawn every configured stdio server for a
turn that then errors and sends nothing. Provider resolution is pure and
cheap, so keeping it first costs nothing and preserves that property
exactly.

Step 3 stays ahead of the status block, which is the ordering rule
`streamTurn` already documents: `mcpStatusSegment` must read post-attempt
state, or a first-attempt failure is reported one turn late. The catalog
segment now reads post-attempt state for the same reason.

The dependency runs ONE way. Step 4 consumes step 3's catalog text, which
is why step 3 must precede it. Step 3 consumes nothing step 4 produces: no
tool source reads `params` or the system slice — `toolDefs` reads
`s.tools`, the MCP registry, and `Hooks.Tools()`, and none of the three
takes the model as input. So the two cannot be swapped, and step 3 needs
nothing from the assembly that follows it.

The reorder moves one hook, and the move is deliberate. `chat.params`
stays first and still fires on every turn, because provider resolution
needs the model it returns. `system.transform` moves from before the tool
plan to after it, so two things change for that hook:

- A turn naming an unconfigured provider now returns WITHOUT firing
  `system.transform`. It used to fire, then fail. Assembling a system
  prompt for a request that is never sent buys nothing, and a plugin that
  counts `system.transform` dispatches now counts SENT requests.
- On a turn that IS sent, `system.transform` now runs AFTER the first MCP
  connect attempt rather than before it. A plugin that inspects live MCP
  state during that hook therefore sees post-attempt state — the same
  correction `mcpStatusSegment` already needed. No hook input changes:
  `system.transform` is handed the session id and the model, never the
  tools array or the system slice.

Both are behaviour changes outside the opt-in path, so they land documented
in AGENTS.md and pinned by a test, not silently.

Shape:

```
Deferred MCP tools. These tools exist but their input schemas are not
loaded. To use one you MUST first load it with the mcp tool:
mcp(action="select", tools=["mcp__github__create_issue"]). A selected tool
appears in your tool list on the next request and is then called directly.
Use mcp(action="search", query="...") to find a tool by keyword.

mcp__github__create_issue — Create a new issue in a repository
mcp__github__list_pull_requests — List pull requests for a repository
```

Each line takes the tool's description, cut at its first line break and
truncated to `mcpCatalogDescriptionMax` bytes on a UTF-8 boundary
(`truncateUTF8`, already in the engine). A selected tool is NOT listed: its
full schema is in the tools array, so listing it again spends bytes twice.

The listing renders in ascending order of the full namespaced tool name,
sorted by the engine and not inherited from the registry's slice order.
The registry sorts by server then tool, which USUALLY agrees but can
differ: for servers named `a` and `a0`, `mcp__a0__b` sorts before
`mcp__a__z` by name, and after it by server. One stated order keeps the
segment byte-stable whatever the registry does, which the byte-stability
test depends on.

The listing is bounded at `mcpCatalogListingMax` entries. Past that bound
the segment prints the first N of that same name-ordered list, then one
trailing line,
`... and N more tools; use mcp(action="search", query="...") to find them`.
A bounded listing keeps a pathological catalog from re-creating the very
problem this design removes.

An ambient `EngineContext` block (`withAmbientStatus`, `engine/process.go`)
was rejected for the listing. That mechanism rides the newest user message,
outside the cached prefix, so the whole catalog would be re-sent uncached
on every turn. The system segment sits inside the cached prefix and is
re-read, not re-written, while the catalog holds still. The degraded-server
block (`mcpStatusSegment`) stays where it is: it is live status, it is
small, and it must correct itself the instant a retry commits.

## 4. The `mcp` tool: `search` and `select`

### Why a verb, not a new tool

Two new actions join the existing `mcp` session tool (`engine/mcp_tool.go`)
rather than a dedicated `tool_search` tool:

- The registration gate is already correct. `newSession` registers `mcp`
  when the session has at least one configured server — exactly the
  condition under which a deferred catalog can exist.
- Every comparable engine surface is one tool with an action enum:
  `goal` (status/set/adjust), `model` (status/set), `process`,
  and `mcp` itself (status/connect). A second MCP tool would be the only
  split surface in the set.
- A new tool def costs a permanent slot in the tools array of every
  session. Deferral exists to spend fewer bytes there.

The argument shape follows harness conventions, not Claude Code's
`select:<name>` query-string form: `action` plus structured arguments the
provider validates against the schema. A single overloaded `query` string
would move parsing (and a whole class of malformed-input errors) into the
tool body for no gain.

The two actions are advertised when `sessionCanDefer` holds. That predicate
walks the CONFIGURED server names and asks whether any one of them could
ever defer:

```
sessionCanDefer = the session holds the mcp tool
                  AND some configured server's policy mode is not eager
```

A server's policy mode is its `MCPToolLoadingByServer` override when set,
and the global mode otherwise. `auto` counts as "not eager" here, because a
catalog can cross the threshold at any moment.

Both halves of the predicate matter, in both directions:

- Reading the global mode alone OVER-advertises. A global `lazy` whose
  every server is pinned `eager` defers nothing, yet would still offer two
  actions with nothing to act on.
- Reading the global mode alone also UNDER-advertises. A global `eager`
  with one per-server `lazy` defers that server and lists its tools as
  selectable, so the model would be told to call an action its schema does
  not offer.

A session that can defer nothing gets today's two-action schema unchanged,
so an existing config sees no new text in its tools array. Every input to
the predicate is session config, fixed for the session's life, so the def
is byte-stable either way — including under `auto`, where the RUNTIME decision
moves with the catalog but the policy does not.

### `search`

```json
{"action": "search", "query": "create issue", "limit": 20}
```

Ranking is deterministic and needs no index. The rules are stated to the
byte, because a golden ranking test must have exactly one right answer.

Normalization, applied once before any scoring:

- Lowercase the query and every field it is matched against, with
  `strings.ToLower`.
- Tokenize the lowercased query on every run of characters outside
  `[a-z0-9]`. Drop empty tokens, then DEDUPLICATE: a token repeated in the
  query scores once, not twice.
- A blank query, or one that yields no token, is an error. `search` never
  answers with the whole catalog.

Scoring, for one tool, against the DEDUPLICATED token set:

| Signal | Points |
|---|---|
| The whole trimmed lowercased query equals the lowercased full name, or the lowercased remote name | 100, once |
| A token is a SUBSTRING of the lowercased remote name | 50 per distinct token |
| A token is a SUBSTRING of the lowercased description | 10 per distinct token |
| A token is a SUBSTRING of the lowercased server name | 5 per distinct token |

Substring, never whole-word: a query token must match inside
`create_issue` and inside `createIssue` alike, and the engine does no
stemming. Each distinct token scores at most once per field, whatever the
number of occurrences. A tool scores in all four rows at once when it
qualifies for all four.

Worked example. Query `create issue`, tokens `[create, issue]`, tool
`mcp__github__create_issue` on server `github`, described as `Create a new
issue in a repository`:

- The whole query, `create issue`, does not equal `create_issue`, so no
  exact bonus.
- Both tokens are substrings of `create_issue`: +100.
- Both are substrings of the lowercased description: +20.
- Neither is a substring of `github`: +0.
- Total: 120.

Results sort by score descending, then by name ascending, so ties are
stable. A zero-score tool never appears. `limit` defaults to
`mcpSearchDefaultLimit` (20), is capped at `mcpSearchMaxLimit` (50), and a
value below 1 falls back to the default.

The result is JSON:

```json
{
  "matches": [
    {"name": "mcp__github__create_issue", "server": "github",
     "description": "Create a new issue in a repository", "loaded": false},
    {"name": "mcp__github__list_issues", "server": "github",
     "description": "List issues in a repository", "loaded": true}
  ],
  "total": 2,
  "truncated": false
}
```

`total` counts every tool that scored above zero, before `limit` cuts the
list. `truncated` is `total > len(matches)`, so the two fields never
disagree.

`loaded` is true for a tool of an eager server, and for a selected tool
of a deferred server that the live catalog holds — the set-and-catalog
test is the single authority. That usually coincides with "the schema is
in the tools array right now", with one deliberate window: a `select` and
a `search` in the SAME assistant message report the just-selected tool
`loaded: true` although its schema only enters the array on the next
request (§1) — the answer describes that next request, which is the one
the model is deciding about. A `pending` selection is not
loaded — no catalog holds it — and it never appears in a result anyway,
since `search` ranks the live catalog only. It deliberately does not report set membership: what the
model needs to know is whether it must call `select` before it calls the
tool, and under `auto` below the threshold — where nothing defers — the
honest answer for every tool is `true`. A `selected` flag would answer
`false` there and send the model into a `select` call it does not need.

`search` covers the whole catalog, deferred or not. It never mutates
state.

### `select`

```json
{"action": "select", "tools": ["mcp__github__create_issue"]}
```

`select` adds names to the session's selected set, journals the addition
(§5), and returns:

```json
{
  "selected": ["mcp__github__create_issue"],
  "already":  [],
  "pending":  [],
  "missing":  [],
  "note": "selected tools are callable from the next request in this turn"
}
```

The `note` is CONDITIONAL on what the batch actually produced, because the
model acts on it:

- At least one `selected` name: "selected tools are callable from the next
  request in this turn."
- Only `pending` names: they are NOT callable next request — their server
  is down — so the note says they load once that server reconnects.
- Neither: no claim about callability at all.

An unconditional "callable next request" would send the model straight into
calling a tool whose server is degraded.

Each name lands in exactly one bucket. The table is evaluated TOP TO
BOTTOM, and the first row that matches wins:

| Bucket | Condition | Journaled |
|---|---|---|
| `already` | The name is in the selected set already, whatever its server's state. | no — it is already durable |
| `selected` | The catalog holds the name. | yes, unless that server's selected mode is `eager` (see below) |
| `pending` | The name's server is configured but not connected. | yes, unless that server's selected mode is `eager` — same gate as `selected` (see below) |
| `missing` | The server is connected and has no such tool, the server is not configured, or the name is malformed. | no |

A name that is not `mcp__<server>__<tool>` shaped, or that has an empty
server or tool segment, is `missing`. It carries no server, so no other
bucket can hold it, and it must not be journaled — the same shape §5's
replay guard already skips. The two guards state one rule at the two ends
of the record's life.

`pending` exists to keep `select` symmetric with reload. A selection
restored by `LoadSession` for an absent server stays in the set and arms
itself the moment that server reconnects (§5). Without `pending`, the same
`select` call made DURING an outage would report `missing` and record
nothing, so a model that selects at the wrong moment gets no tool and no
signal. The result names the state, and the ambient degraded-server block
(`mcpStatusSegment`) already names the reason.

`pending` accepts a name; it does not help the model FIND one. `search`
ranks over the live catalog, and a server that has never connected in this
process has no catalog to rank — `mcpServerEntry.Connected` is a one-way
latch (`engine/mcp.go`), so an unconnected server is one whose first
attempt never succeeded, not one that dropped after working. The model
therefore reaches `pending` only with a name it already holds: from its own
history after a restart, or from the user. Discovery during an outage is
`mcp(action="connect", server=...)`, the explicit re-trigger that already
exists — connect the server, then search its catalog. A cached
last-known-good catalog for an unconnected server would create a second
discovery path, and a second source of stale names; it is a non-goal
(§10).

`missing` is never journaled. A name for a connected server that does not
hold it is a typo or an invention; recording it would let a hallucinated
name live in the session's durable state for good.

`pending` cannot close that hole at select time — a parked server's catalog
is unknown, so an invented name and a real one are indistinguishable — so
it closes it at REAP time instead. Whenever the plan runs, a selected name
is dropped from the session's set when its server is CONNECTED and the
catalog does not hold the name. A real tool arms itself; an invented one is
reaped on the same event. The reap is memory-only and needs no removal
record: replay unions the log again on the next reload, and the same rule
prunes the same name again on that session's first plan. The durable log
therefore keeps one inert line per `select` call, and the EFFECTIVE set
never exceeds the live catalog.

The reap is deliberately blind to WHY a connected server does not hold a
name. A server that drops a tool from its own catalog reaps that selection
too, which is correct: the tool is gone, the model re-selects if it comes
back.

Rules:

- An empty or absent `tools` array is an error. Every other input shape
  yields a result, never an error, so one bad name never voids a batch of
  good ones.
- **Both writers of the record apply the same per-server gate.** `select`
  records a name — in memory and in the journal — only when that name's
  server COULD ever defer, which is exactly the condition
  use-implies-selection uses (§3). The two writers must not disagree: a
  record exists only to survive a flip to deferred, so the question "can
  this server ever flip" has one answer, whichever writer is asking.
  - A server whose selected mode is `auto` — resolving eager today because
    the catalog is under the threshold — CAN flip. Its tool is `selected`
    and journaled, and that record is what keeps it loaded across the flip
    and across a reload.
  - A server whose selected mode is `eager` can NEVER flip, so its tool is
    reported `selected` — it is loaded and callable, which is what the
    model asked for — but nothing is recorded, because the record could
    never pay for itself. A repeat `select` reports `selected` again rather
    than `already` — honest, since no set membership made the tool loaded —
    UNLESS the name is already in the restored selected set (a session
    created under `lazy`, selected, then reloaded under `eager` keeps its
    set, and the bucket order tests `already` first): then it reports
    `already`, equally honest.

    Two configurations reach that state, and the rule covers both: a server
    pinned `eager` by `MCPToolLoadingByServer`, and a server that simply
    INHERITS a global `eager` in a session made defer-capable by some other
    server's `lazy` pin. Reading the rule as "pinned only" would record for
    the second, which can no more flip than the first.

  Either way the call is cheap by construction: the def is in the array
  before and after, so the array's bytes do not move and no cached prefix
  is invalidated. The waste is one tool round trip, and the `loaded` flag
  on every `search` result exists to stop the model spending it.
- Only the namespaced form is accepted. `search` returns namespaced names,
  so the model always holds one, and a bare remote name is ambiguous across
  servers.
- `select` does NOT echo schemas. The tools array is the one authoritative
  copy. Echoing would write every schema a second time into DURABLE
  history, where it is re-sent on every later turn of the session — the
  opposite of this design's purpose.

There is no `deselect`, and the model cannot shrink the set. Two different
statements hold, one per layer, and conflating them into "selection is
monotone" would contradict the reap defined above:

- The DURABLE record set is append-only. Nothing ever retracts a written
  `mcp.tools_selected` record.
- The EFFECTIVE in-memory set is bounded by the live catalog, and the reap
  shrinks it: a name whose server is connected without it is dropped, on
  every plan, replay after replay.

A release action would also race a tool call the model has already emitted
in the same turn. It is a candidate for later work, not v1.

## 5. Persistence and recovery

The selected set has TWO writers, and both write the same record. `select`
writes one record per call that adds names (§4). A routed MCP tool call
writes one when its own name enters the set, in a session that can defer
(§3, "Use implies selection"). An implementation that wires journaling into
`select` alone loses the flip-survival guarantee that second writer exists
for.

The record:

```json
{"type": "mcp.tools_selected", "seq": 42,
 "mcp_tools": ["mcp__github__create_issue"]}
```

`recMCPToolsSelected` (`engine/store.go`) follows `recToolResultRetained`:
engine-internal session state, journaled and folded, with no engine event
and no server journal mapping. Selection is not a lifecycle transition a
dashboard renders; it is state a resumed session must not lose.

One record covers every name a `select` call adds, `selected` and `pending`
together (§4): the two differ only in whether the tool is reachable right
now, and the durable state has no reason to keep them apart. The §4 gate
applies unchanged here: a `selected`-bucket name whose server's resolved
mode is `eager` (pinned or inherited) is loaded and callable but is NOT
part of the record — it can never flip, so there is nothing to restore.

`LoadSession` unions every such record, in log order, into the restored
session's selected set. Replay is defensive, like `recPromptQueued`'s: a
name that is not `mcp__<server>__<tool>` shaped is skipped, and duplicates
collapse.

Recovery degrades in one direction only:

- A restored name whose tool is present again contributes its schema on the
  first request after the reload. The model needs no re-select.
- A restored name whose server is absent or parked contributes nothing and
  STAYS in the selected set. The state is small, and keeping it makes the
  recovery self-healing the moment that server reconnects.
- A restored name whose server IS connected without it is reaped from the
  set on the first plan after the reload (§4). Nothing durable is written;
  the same rule prunes the same name after every later reload.
- A session created under `lazy` and reloaded under `eager` keeps the
  restored set; it is inert, because every tool is registered anyway.

## 6. Prompt-cache behaviour

A `select` changes the tools array. That invalidates the provider's cached
prefix for the next request. No design avoids it once a selected tool
becomes a real tool def. It is the correct trade: one invalidation per
selection event, against a permanently larger prefix on every turn.

Two properties bound the cost:

- Batching. `select` takes an ARRAY, and both the tool description and the
  catalog header tell the model to select everything it needs in one call.
- Stability between selections. A request that changes no selection
  produces a byte-identical array, so the steady state is a cache read.

The catalog segment obeys the same rule: identical bytes while the catalog
holds still, one prefix rewrite when a server connects or drops. An eager
session already pays exactly that on a server connect, because its tools
array grows at the same instant.

## 7. Failure and degrade paths

- **A call to a tool that is not in the array.** `executeTool`
  (`engine/engine.go`) routes any `mcp__`-prefixed name to
  `MCPRegistry.CallTool`, which resolves the binding from the live merged
  view. This stays open. It is the safety net for an `auto` flip mid-turn
  and for a model that replays a name from earlier history. A provider does
  not invent calls to undeclared tools, so this is a recovery path, not the
  main one.
- **An unconnected server.** Unchanged. Its tools are in no catalog,
  `mcpStatusSegment` names it as degraded, and `mcp(action="connect")`
  remains the explicit re-trigger. Once it connects, its tools appear on
  the very next request — in the catalog listing if deferred, or in the
  tools array with full schemas if the server's resolved mode registers
  them eagerly (`eager`, or `auto` below the threshold).
- **`select` for a tool of a degraded server.** The name is `pending`: it
  is journaled, and it arms itself when the server reconnects (§4). The
  ambient status block already tells the model why the server is down.
- **A child session that cannot select.** Deferral is off for it entirely
  (§3), so it keeps eager registration.
- **No MCP servers configured.** No `mcp` tool, no catalog segment, no
  change of any kind.

## 8. Subagents

Selection is per-session state. A child spawned through `task`
(`engine/session_manager.go`) starts with an empty set and re-selects what
it needs. Inheriting a parent's set would ship schemas the child's own task
may never touch, which is the cost this design removes.

A child whose agent definition drops the `mcp` tool defers nothing at all
(§3). It keeps eager registration, because a session that cannot call
`select` must never be shown a catalog it cannot load.

The agent-definition tool restriction is unchanged, and it is worth
recording what it does today: `restrictTools` narrows `Session.tools`, the
built-in and config tool map. MCP tools do not live there — `toolDefs`
appends them from the registry — so an agent definition's `tools:` list
never filtered MCP tools, before this change or after it. Deferral does not
alter that reach. A restriction that covers MCP names is a separate change
with its own design.

## 9. Verification

Each slice lands with its own tests. No test sleeps; timer-free by
construction, since nothing here has a schedule.

1. **Deferral core.** Effective-mode table test across global mode,
   per-server override, and the `auto` threshold boundary (at, one below,
   one above), with a server pinned `eager` still counted in the total. A
   session with no `mcp` tool defers nothing, even under
   global `lazy` — the subagent-lockout guard. Tools-array partition: a
   deferred server contributes no def, an eager server contributes every
   def. Byte-stability: repeated `toolDefs` calls with no selection change
   marshal identically, the assertion shape
   `engine/tooldefs_order_test.go` already uses. Catalog segment: exact
   text, listing bound, description truncation, and the position of the
   segment in the assembled system slice, asserted on the slice
   `Session.Prompt` actually sends. Listing order is by full tool name, so
   a registry slice in a different order renders identical bytes. A turn
   naming an UNCONFIGURED provider triggers no MCP connect — red-verify
   against a plan computed at the top of `streamTurn`.
2. **Search and select.** Ranking table test driven from the normalization
   and scoring rules in §4, including the worked example's exact total, a
   repeated query token scoring once, a substring hit inside a compound
   name, tie-break by name, the zero-score exclusion, the blank-query
   error, and `total`/`truncated` agreeing when `limit` cuts the list. `select` partition across all four
   buckets, with the journal write asserted for `selected` and `pending`
   and asserted ABSENT for `already`, `missing`, and a tool whose server's
   selected mode is `eager` — both by a pin and by inheriting a global
   `eager` in a defer-capable session. The `note` is asserted per batch shape:
   selected-bearing, pending-only, and neither. Empty `tools` errors.
The action gate, in both directions: a global-`eager` session
   with one per-server `lazy` advertises `search`/`select`, and a global-
   `lazy` session whose every server is pinned `eager` does NOT —
   red-verify both against a global-mode-only gate. Bucket order: a
   restored name whose server has since parked is `already`, never a second
   `pending` record. A malformed name is `missing` and journals nothing.
   Use implies selection: a routed call to a tool that was never selected
   loads its schema for the next round, and a repeat call writes no second
   record. The gate is per server: a session that can defer nothing records
   NOTHING on a routed call, and neither does a defer-capable session on a
   call to a server pinned `eager`. The in-turn effect: a fake provider that calls
   `select` on round 1, and an assertion that round 2's
   `provider.Request.Tools` carries the schema — driven through
   `Session.Prompt`, the production entry point, never by calling
   `toolDefs` by hand.
3. **Persistence and recovery.** Both writers journal the same record: a
   `select` call and a routed first call to an unselected tool each produce
   one, and each survives a reload. A journal round trip through
   `LoadSession`: selection survives, malformed and duplicate names are
   skipped, and a restored name whose tool is gone yields no def and no
   error. A `pending` selection made during an outage arms itself once the
   server connects, without a second `select`, and an INVENTED pending name
   is reaped on that same event — the pair that proves the reap rule closes
   the hallucinated-name hole without breaking self-heal. Red-verify each
   guard against the pre-fix code.
4. **End-to-end.** An `httptest` MCP server (the `fakeMCPHTTPServer` shape
   in `engine/mcp_test.go`) serving a catalog above the threshold, driven
   through a real `MCPManager` and a real `Session`: the first request
   carries the catalog segment and no MCP schemas, `select` runs through
   the real tool, and the next request carries exactly the selected
   schema, which the model then calls through `CallTool` to the same
   server. In-process transport, per the testing rules — the subprocess
   machinery is not what is under test.

## 10. Non-goals

- **No provider-side tool search.** Anthropic's server-side tool-search
  beta solves this inside one vendor's API. Harness is provider-agnostic,
  and a per-provider mechanism would not serve the openai, openaicompat,
  or gemini routes.
- **No deselect, no eviction, no TTL** on a selected tool (§4).
- **No cached catalog for an unconnected server.** `search` ranks over
  live tools only (§4).
- **No semantic or embedding search.** Keyword ranking is deterministic,
  needs no model call, and costs no startup budget.
- **No change to MCP connection lifecycle.** Lazy connect, bounded
  background retry, parking, and the `connect` action are untouched.
- **No filtering of MCP tools by agent definition** (§8).
- **No deferral of plugin tools.** Plugin catalogs are small and
  locally declared. The mechanism would generalize; the need does not
  exist yet.
