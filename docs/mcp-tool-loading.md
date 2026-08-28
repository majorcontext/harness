# MCP tool loading

This document describes deferred MCP schemas and deterministic tool ordering.

## Lazy MCP tools (deferred schemas)

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
pressure. A zero threshold resolves to the default. User/project config
rejects a negative threshold; a direct engine embedder that supplies a
non-positive `Engine.Config` value also receives the default, never a floor
of 1 — that value would defer every catalog.

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

## The tool array is byte-stable across requests

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
