# Go plugins

Go programs can write Harness plugins. One plugin value runs two ways:

- **In-process:** an embedder passes it to `harness.New` through `Options.Plugins`. No subprocess starts.
- **Out-of-process:** a small `main` calls `plugin.Serve`. The result is a plugin process that `harness run` and `harness serve` load from the `plugins` config key, like a TypeScript plugin.

A plugin author can also mark a `tool.execute.before` hook as deny-on-failure. When that hook fails, Harness blocks the tool call instead of running it.

User and project plugins merge, and the most trusted plugins run last. A project config can no longer remove a user plugin or rewrite a tool call after a user guard checks it.

## Why

- Today only the TypeScript SDK is public. The Go helpers in `internal/plugin` are a test fixture that no other module can import.
- An embedder can add tools through `Options.Tools`, but it cannot intercept a tool call, change the system prompt, set shell environment, or watch events without a subprocess.
- Hook chains fail open. A guardrail plugin that crashes or misses its deadline lets the call it guards run. A safety plugin needs a way to fail closed.
- A non-empty project `plugins` array replaces the user array (`config/merge.go`). A cloned repository's `.harness.json` that lists any plugin removes the user's guardrail. A project plugin that runs after a guard can also rewrite the arguments the guard checked. Claude Code, Codex CLI, and Cursor all add hooks across config levels, and none lets a project remove a user hook.

The proving case is a guardrail. It blocks `git push --force`, `rm -rf` on broad targets, and reads of `.env` files. It must work in-process and as a process from the same code, and it must deny when it fails.

## Goals

1. A public package `github.com/majorcontext/harness/plugin` with the author-facing API: the plugin value, its hooks, its tools, the client handle, and `Serve`.
2. `harness.Options.Plugins` registers plugin values in-process.
3. The same hook rules apply in both modes: config order, the dispatch deadline, fail open by default, and deny-on-failure when marked.
4. A per-hook deny-on-failure flag, set by the author, carried in the manifest. It is valid only on `tool.execute.before`.
5. The TypeScript SDK can set the same flag.
6. `examples/guardrail` runs in both modes.
7. User and project plugins merge. The chain runs project plugins, then user plugins, then in-process plugins.

## Non-goals

- **Middleware hooks** (a handler that wraps a step and calls `next`). Hooks stay request/response. A later design can add them.
- **Richer events** for telemetry: timestamps, model-call and turn events, links between events, trace-header injection.
- **New hooks** or a changed hook set. `chat.message` stays undispatched, as `docs/architecture.md` states.
- **An operator override of deny-on-failure.** The author decides. The person who installs a plugin cannot change it.
- **A permission or approval system.** Deny-on-failure is a failure policy of one plugin hook, not a user-facing approval step. The root `AGENTS.md` non-goal stands.
- **Trust gating of project plugins** (a prompt or a hash check before a project plugin runs, as in Codex CLI and Gemini CLI). It is close to the approval-system non-goal and needs its own decision.
- **A way for a project to disable a user plugin.**

## Anti-goals

- A flagged guardrail that fails open in any path. A stale manifest cache, a plugin that never starts, a crash, a timeout, or a panic must all deny.
- A less trusted plugin that runs after a more trusted one, for any hook.
- Two behaviors for one plugin. In-process and out-of-process runs must chain, time out, and fail the same way.
- A public API that exposes an internal type. An author outside this module must be able to compile every signature.
- A panic in an in-process hook that crashes the embedder.

## Design

### Package layout

- `plugin/` (public): the author API. It holds the plugin value, hook function types, request and response types, tool types, the client handle, `Serve`, and the wire types that an author sees.
- `internal/plugin` (host): the host, the transport, the probe, and the manifest checks. It imports `plugin/` for the shared wire types.
- The JSON-RPC transport is `internal/plugin/wire`. Both `Serve` and the host use it, and it is not public.
- `internal/gates/deleted.go`: remove `plugin` from `deletedPathRE`. The pattern bans the path because the package moved under `internal/`; this design makes the author API public again.
- `docs/architecture.md`, Public packages: add a `harness/plugin` row. The `checkPublicAPI` gate compares this table with the exposed packages.

### No internal types in the public API

Today the author types use `internal/message` (`message.Parts`) and `internal/modelref` (`ChatParams.Model`). The public package must not expose them.

- A model reference is a `string` in `provider/model` form.
- Message content uses public types. Text parts and `blob` parts (binary content such as an image) have typed fields. Every other part, provider-tagged opaque parts included, keeps its raw JSON, so it round-trips unchanged. Reuse `protocol` types where their JSON equals the v1 wire JSON. Otherwise define the types in `plugin/`. The host converts at its boundary.
- The v1 wire JSON does not change. A Go plugin and a TypeScript plugin see the same bytes.
- Update the `message-consumers` depguard list in `.golangci.yml` if a package that imports `internal/message` changes.

### The plugin value

A sketch. The builder settles exact names.

```go
var Guard = &plugin.Plugin{
	Name:    "guard",
	Version: "0.1.0",
	ToolExecuteBefore: &plugin.ToolBefore{
		DenyOnFailure: true,
		Run: func(ctx context.Context, c *plugin.Client, req *plugin.ToolBeforeRequest) (*plugin.ToolBeforeResponse, error) {
			if req.Tool == "bash" && isForcePush(req.Args) {
				return &plugin.ToolBeforeResponse{Deny: "force push is blocked"}, nil
			}
			return nil, nil
		},
	},
}
```

- The hooks are the dispatched hooks of today: `event`, `chat.params`, `system.transform`, `shell.env`, `tool.execute.before`, `tool.execute.after`, and tools.
- The manifest comes from the value: the hooks that are set, their flags, and the tool definitions. An author cannot write a manifest that disagrees with the code.
- `DenyOnFailure` exists only on the `tool.execute.before` hook type, so the compiler rejects it elsewhere.
- Hooks and tools must be safe for concurrent use. The host overlaps calls: a batch of tool calls runs in parallel, and events arrive asynchronously. The `Plugin` doc comment states this.
- One `*Plugin` value can serve several runtimes at once. Each runtime runs its own `Serve` over its own pipe. State that the author keeps in package variables is shared.

### In-process mode

```go
rt, err := harness.New(harness.Options{
	Store:   store.NewDiskStore("sessions"),
	WorkDir: ".",
	Getenv:  os.Getenv,
	Plugins: []*plugin.Plugin{guard.Guard},
})
```

- The runtime connects each in-process plugin to the host through the existing `Spec.dial` seam, over an in-memory pipe that runs `Serve`. The host then applies its chain, deadline, fail-open, and deny-on-failure code to both modes. This is the parity guarantee.
- The cost is JSON encoding and one goroutine for each call. That is acceptable for a first version. A direct-call path can come later if a measurement shows a need, and it must keep the same rules.
- No probe and no manifest cache. The host reads the manifest from the value.
- Order: in-process plugins run last, in `Options.Plugins` order. See "Config merge and order".
- A name must be unique across both groups. `harness.New` fails on a duplicate name.
- `pluginsrc` builds one spec list: the config plugins first, then the in-process values. It exists when either list is non-empty.
- Tool names follow the existing `known` rule. `harness.New` fails when an in-process tool collides with a built-in tool, an `Options.Tools` tool, or another in-process tool. A collision with a config plugin tool is found at the first `Create` or `Open`, where config plugin collisions are found today.
- The plugin inventory lists in-process plugins with their state, beside config plugins. `Runtime.ProbePlugins` probes only config plugins.
- `Plugins` works with or without a `WorkDir`. A hook that needs a working directory sees the empty value, as a config plugin does today.

### Out-of-process mode

```go
// cmd/guard/main.go
func main() { plugin.Serve(guard.Guard) }
```

```json
{ "plugins": [{"name": "guard", "command": ["guard"]}] }
```

- `Serve` reads stdin and writes stdout. It logs to stderr. It returns when the host sends `shutdown` or closes stdin.
- `Serve` is the public form of `internal/plugin`'s unexported `serve`.

### Deny on failure

The manifest gains an optional field. `hooks` keeps its shape, so a v1 reader that ignores the new field still works:

```json
{"name": "guard", "protocol_version": 1,
 "hooks": ["tool.execute.before"],
 "deny_on_failure": ["tool.execute.before"]}
```

Rules:

- `deny_on_failure` is valid only for `tool.execute.before`, and only when `hooks` also lists it. Any other value makes the manifest invalid: a probed manifest fails the first `Create` or `Open`, and an in-process plugin fails `harness.New`.
- A failure is any of: the plugin does not start, the process exits, the dispatch deadline passes, the response does not decode, the hook returns an error, or the hook panics.
- On a failure of a flagged hook, the host stops the chain and denies the call. The model gets an error tool result that names the plugin and the failure, for example `denied: plugin guard failed: deadline exceeded`.
- The deadline is the hook timeout (see Hook timeouts). A flagged hook that hangs denies at the deadline. It never wedges the session.
- A plugin that fails to start stays failed for the host lifetime, unless the caller canceled the call that started it; the next call then starts it again. Each later call that a flagged hook would see is denied.
- An unflagged hook keeps the current behavior: skip the plugin and continue the chain.
- A canceled context is an abort, not a failure. A user interrupt or `Runtime.Close` during a flagged hook ends the call as it does today. It records no deny.
- `Options.OnError` of the host still receives each failure, flagged or not.

**Known limit: an older host fails open.** A host from before this change ignores `deny_on_failure`. No plugin-side check can change that: a protocol version bump or a capability check only makes the plugin fail to start, and an older host skips a plugin that fails to start. A guardrail that needs fail-closed behavior needs a host with this change. `PROTOCOL.md` states this limit.

### No stale-cache bypass

At spawn, the host compares only `Name` and `ProtocolVersion` of the live manifest with the cached one. A cache entry from before this change has no `deny_on_failure`, so a flagged guardrail could run unflagged.

- At spawn, compare the full manifest: hooks, `deny_on_failure`, and tool definitions. A mismatch is a start failure.
- A hook is flagged when the cached manifest or the live manifest flags it. A start failure, a mismatch included, then denies that hook's calls.
- The cache entry format gets a version. An entry without the current version is a miss, so an entry from before this change is probed again.
- A flagged plugin with no usable cache entry and a failed probe keeps today's behavior: `Create` and `Open` fail.

### Config merge and order

**Merge.** The user `plugins` array and the project `plugins` array both load. A project value no longer replaces the user value.

- The same plugin name in both files is a config error that names the plugin and both files. Neither file silently replaces the other's plugin.
- An empty or omitted project array adds no plugins. It does not disable the user plugins.
- The same name in `Options.Plugins` and in config fails `harness.New`.

**Order: the most trusted plugins run last.** For each hook, the chain runs:

1. project plugins, in project order;
2. user plugins, in user order;
3. in-process plugins, in `Options.Plugins` order.

A later plugin sees the changes of the earlier plugins, so the last plugin has the last word:

- `tool.execute.before`: a user guard sees the final arguments after every project rewrite. An embedder guard sees them after the user plugins too.
- `tool.execute.after`: a user redactor runs last, so no project plugin adds unredacted output after it.
- `chat.params` and `shell.env`: the more trusted tier sets the final values.
- `system.transform`: segments append in chain order.
- A deny stops the chain. A project plugin can still deny a call. It cannot allow a call that a later plugin denies, because the later plugin still runs.

**Trust argument.** `config/AGENTS.md` allows an additive key only with a trust argument like the one for `append_system_prompt`. For `plugins`:

- Today a project can already start any plugin process by listing it. Merging gives a project no new power.
- Merging removes one power that a project should not have: to drop a user plugin.
- The order keeps project code from overriding user code, and user code from overriding the embedder.

Update the `config/AGENTS.md` bullet to name `plugins` as the second additive key, with this order. `mergeSpecs` must keep the origin of each spec, or produce the merged list in chain order, so the runtime does not need to know which file a spec came from.

**Cache and probe.** The manifest cache key does not include the origin, so a plugin listed in either file keeps its cache entry. `harness plugin probe` lists the plugins in chain order.

### Panics

- `Serve` recovers a panic in a hook or tool and returns it as an error response. In-process, this keeps a panic out of the embedder's goroutines. Out-of-process, the plugin keeps running.
- The host treats that error like any hook error: fail open, or deny when flagged.

### Hook timeouts

The default dispatch deadline stays at 5 seconds. The plugin author can set a longer or shorter deadline for each hook in the manifest:

```json
{"name": "guard", "protocol_version": 1,
 "hooks": ["tool.execute.before"],
 "hook_timeouts": {"tool.execute.before": 60000}}
```

- `hook_timeouts` maps a hook name to a deadline in milliseconds. It is optional; a hook with no entry uses 5 seconds.
- A value must be from 1 to 600000 (10 minutes), and the hook must be in `hooks` and must not be `event`. Any other value makes the manifest invalid: a probed manifest fails the first `Create` or `Open`, and an in-process plugin fails `harness.New`.
- The full manifest comparison at spawn includes `hook_timeouts`. A difference is a start failure, as for the other fields.
- The timer does not pause while the hook waits on a client call that it made.
- When the dispatch of that hook starts the plugin, the `initialize` call has the longer of the hook timeout and the 5-second default, so a short timeout never shortens the start. Only `initialize` has that bound; the spawn or dial does not end with the context. The hook timeout bounds the hook call alone.
- Go: `Plugin.HookTimeouts map[Hook]time.Duration`; `Manifest()` converts each value to milliseconds. TypeScript: `createPlugin({ hookTimeouts: { toolExecuteBefore: 60000 } })`.

### The client handle

- The Go API can do all that the TypeScript SDK can do. The Go `Plugin` has a field for each TypeScript hook, `ChatMessage` included. Its doc states that the runtime does not dispatch `chat.message`.
- The public `Client` offers each item of the TypeScript `PluginContext`: `WorkspaceDir`, `Config`, `HTTPHeaders`, `HTTPClient` (an `*http.Client` that adds `HTTPHeaders` to each request; a header that the request sets wins), `SessionMessages`, `MCPCall`, and `Generate`. It also offers `HarnessVersion`, `ServeURL`, and `RunToken`.
- `MCPCall` and `Generate` send `client/mcp.call` and `client/generate`. The host serves both (see Host client calls).

### Host client calls

The host serves `client/mcp.call` and `client/generate`. The wire request shapes do not change.

`client/mcp.call`:

- The plugin can reach each server in the `mcp_servers` config of the runtime. A session's tool filter does not apply, because the call has no session.
- The host waits for the first connect of the servers and never dials again. A server that is retrying in the background or parked fails the call at once with its reason and no new attempt; only the connect action of the `mcp` tool brings a parked server back.
- An unknown server name fails the call with an RPC error. A tool result with `is_error` is a result, not an RPC error. A transport error fails the call, and its text hides the server URL and headers, as for a session tool call.
- The response is `{content, is_error}`, with `content` as message parts.

`client/generate`:

- The host resolves an empty `model` to the default model and an alias to its model. A model that no provider serves fails the call, and so does a model whose backend runs its own agent loop (`client/generate: model <ref> runs its own agent loop; use a model API provider`), because that backend would start an agent with every tool of its CLI. The failure text is masked as a session error is.
- The call is one model request with no tools. The host uses the provider layer of the runtime, with its credentials and retry limits. The plugin never sees a key.
- The response is `{message, usage}`. `message` is an assistant message with the text of the reply. `usage` has the token counts of the call, in the shape of the turn usage that the runtime reports.
- No session records the call. On success the host logs one line with the plugin name, the model, and the usage. A plugin that wants the usage in a session puts it in its tool result.

Both calls:

- A call ends when its plugin connection ends or the runtime closes.
- A hook that makes a client call is still bound by its own hook timeout.
- A client call runs no hook of the chain.
- An in-process plugin reaches the same host client API over its pipe.

### TypeScript SDK

- `createPlugin` accepts a way to flag `toolExecuteBefore`, for example a `denyOnFailure: ['toolExecuteBefore']` field on the definition.
- `manifest()` emits `deny_on_failure`.
- `sdk/AGENTS.md` requires the Go host, `PROTOCOL.md`, and the TypeScript SDK to change together.

### Docs and spec

- `docs/architecture.md`:
  - The Public packages table gets a `harness/plugin` row, and the `harness` row gets `Options.Plugins`.
  - The `Options` block gets the `Plugins` field.
  - The `plugins` section states fail open, deny-on-failure, the in-process mode, the config merge, and the order.
  - The `config` section states that `plugins` merges, and the merge order.
- `internal/plugin/PROTOCOL.md`: the manifest field, the failure rules, the older-host limit, and the removal of "Go fixture" wording. Correct the `client/mcp.call` text: the code and `docs/architecture.md` say it fails, and `PROTOCOL.md` says it routes to `mcp_servers`.
- `docs/plugins-and-protocols.md`: the Go SDK, the in-process mode, and deny-on-failure.
- `internal/plugin/AGENTS.md`: its first bullet says a plugin is a separate process. Change it to allow the in-process mode.
- `sdk/typescript/README.md`: the flag, and the removal of "Go fixture" wording.
- `config/AGENTS.md`: name `plugins` as the second additive key, with the trust argument.
- `config/config.go`: the doc comment of `Plugins`, which states the replace rule today.
- `README.md`: no change. Plugins stay out of the README for now.
- `CHANGELOG.md`: one entry.

## Tests

The root `AGENTS.md` asks for contract rows first. Name the failure for each row before writing it.

- **Out-of-process deny-on-failure.** A fixture plugin flags `tool.execute.before` and then exits, hangs past the deadline, or returns an error. The bash call must not run, and the model must get the deny result. Without the change, the call runs. One row, with a case for each failure kind.
- **Unflagged failure still fails open.** The same failures without the flag let the call run. This pins the default.
- **Invalid flag.** `deny_on_failure` on another hook fails the first `Create` or `Open`.
- **Stale cache.** A cache entry without the flag and a live manifest with the flag is a start failure. The hook counts as flagged, so it denies. An entry without the current format version is probed again.
- **Cancellation.** A user interrupt during a flagged hook records no deny.
- **Merge.** A user plugin and a project plugin both run. Without the change, only the project plugin runs. A table test in `config` covers the merged list, the order, the duplicate-name error, and an empty project array. `config` is a pure package, so its table tests are allowed.
- **Order.** A project plugin rewrites a bash command into a force push, and a user guard after it denies the call. Without the change, the project plugin replaces the guard and the call runs.
- **In-process parity.** The guardrail as an `Options.Plugins` value denies a force push, and denies when its hook panics or exceeds the deadline. This row is Go-API only, like the `Options.Tools` rows. List the file in `testdata/test-exceptions.txt` with its reason.
- **Hook timeout.** A fixture hook that sets a 10-second timeout and takes 7 seconds is not cut off; without the change it fails at 5 seconds. A value over 600000 or on `event` fails the first `Create` or `Open`.
- **MCP call.** A fixture tool calls `client/mcp.call` on a mock MCP server from `harnesstest`, and the tool result holds the server's reply. An unknown server fails the call.
- **Generate.** A fixture tool calls `client/generate` against the mock provider, and the tool result holds the reply text and the usage.
- **TypeScript.** Extend the conformance test so a TypeScript plugin that sets the flag produces the manifest field.
- **The guardrail example.** It builds, and it runs in both modes.

Use `testing/synctest` for deadline cases. Do not use `time.Sleep`.

## Constraints

- `internal/plugin/host.go` is 808 lines, over the 800-line gate. New host code goes in new files, and `host.go` must not grow.
- Functions stay at 80 lines or fewer. Exported identifiers get one-line doc comments.
- Prose in docs follows ASD-STE100, as the root `AGENTS.md` asks.
- Startup: an in-process plugin must not start a goroutine or do work before the first hook dispatch, which matches the lazy spawn of a process plugin.

## Left to the builder

- Exact names of the public types and hook function signatures.
- Where the shared transport lives and how `plugin/` and `internal/plugin` split the wire types.
- Whether message content reuses `protocol` types or gets new types in `plugin/`.
- The fixture changes needed for the failure rows, in `harnesstest/pluginfixture`.

## Decisions

Decided by Andy:

- Both modes, with one plugin value.
- The config merge is part of this design. User and project plugins merge.
- The most trusted plugins run last: project, then user, then in-process.
- Request/response hooks. Middleware and richer events are deferred.
- Deny-on-failure is opt-in, per hook, and set by the plugin author. No operator override.
- The guardrail is the proving plugin.

- The Go API can do all that the TypeScript SDK can do.
- The host serves `client/mcp.call` and `client/generate`.
- `client/generate` usage goes to the runtime log and to the response, not to a session.
- A plugin can set a per-hook timeout in its manifest, up to 10 minutes.
- `client/mcp.call` reaches every MCP server of the runtime.

Calls made in this design, to check:

- The public path is `harness/plugin`, with the deleted-path gate pattern changed.
- In-process plugins use the existing pipe seam for parity, not direct calls.
- The same plugin name in both config files is an error.
- An empty project array does not disable user plugins, and no mechanism lets a project disable one.
- Trust gating of project plugins is out of scope.
- Deny-on-failure is valid only on `tool.execute.before`.
- `deny_on_failure` is a separate manifest list, so `hooks` keeps its v1 shape.
- The full manifest is compared at spawn, and a flag on either side counts.
- The cache format gets a version, so older entries are probed again.
- A canceled context is an abort, not a deny.
- An older host fails open, and the docs state it. No mechanism tries to prevent it.
- Message content: typed text and `blob` parts, raw JSON for every other part.
- Hooks must be safe for concurrent use. One plugin value can serve several runtimes.
- In-process tool collisions are found at `harness.New`, except against config plugin tools, which are found at the first `Create` or `Open`.
- The Go API matches the TypeScript SDK: every hook, `ChatMessage` included, and every client method, `MCPCall` and `Generate` included.
