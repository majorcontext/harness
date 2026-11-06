# Embedding

The runtime is a Go package. The CLI and the HTTP server are clients of it.
Add it to your module:

```bash
go get github.com/majorcontext/harness@latest
```

## Run a prompt

Create a runtime and a session, send the session a prompt, and read the events
of its turn:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func main() {
	ctx := context.Background()
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	rt, err := harness.New(harness.Options{
		Store:   store.NewMemStore(),
		DisableRetryAfterOutput: true,
		WorkDir: ".",
		Getenv:  os.Getenv,
		Config: config.Config{
			Model:         "codex/gpt-6.1-sol",
			CodexAuthFile: filepath.Join(home, ".harness", "auth", "codex.json"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt.Close(ctx) }()

	s, err := rt.Create(ctx, protocol.CreateSession{})
	if err != nil {
		log.Fatal(err)
	}
	turn, err := s.PromptStream(ctx, protocol.Input{
		ID:    "prompt-1",
		Parts: []protocol.Part{{Type: protocol.PartText, Text: "List the files in this directory."}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer turn.Close()
	for e, err := range turn.Events(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		if e.Kind == protocol.KindItemDelta {
			var f protocol.ItemFrame
			if err := json.Unmarshal(e.Data, &f); err != nil {
				log.Fatal(err)
			}
			if f.Type == "text" {
				fmt.Print(f.Text)
			}
		}
	}
	fmt.Println()
	res, err := turn.Wait(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if res.StopReason == protocol.StopFailed {
		log.Fatal(res.Error)
	}
}
```

The program uses the login of `harness login codex`. The runtime reads no
config file, and `CodexAuthFile` names the login file; it has no default in Go.
For Claude Code, set `Model: "claude-code/sonnet"` and no `CodexAuthFile`: the
runtime runs the `claude` CLI, which keeps its own login. For an API key, set
`Model` to an `anthropic/` or `openai/` model. `Getenv` lets the runtime read
the API-key variables. A non-empty selected key, configured auth, or a non-empty
`Authorization` or `x-api-key` credential header makes a native API-key provider
available. Without credentials, `Create` rejects it with `model_unavailable`
before a turn starts, also when the session selects the default model.
The error names the missing key variable. If `ModelTransport` supplies the
credentials, set `Auth: config.AuthInjected` on that provider entry.
A base URL, `WithProxyBase`, or transport alone does not make it available.
The later snippets show `Options`
without `Config`; add the same `Config` to each. `protocol.CreateSession{Model: ...}`
picks the model of one session; empty selects `Config.Model`. `WorkDir` gives each session
the built-in tools, bash among them. Leave it empty for a session with no file
access. A failed turn does not end `Events` with an error, so check
`turn.Wait`.

The Go API keeps the built-in default model, `anthropic/claude-fable-5`, and does
not look for a subscription login. An embedder that wants the subscription
default sets `Config.SubscriptionDefault`.

## Cancellation and cleanup

`Turn.Cancel` and `Runtime.End` cancel the context passed to custom `Tool.Run` calls. The runtime stops waiting even if a custom tool ignores that context. A result accepted before cancellation stays in the log; a late result is discarded. Go cannot kill the tool goroutine. Detached tool code can still have side effects after the session ends, so the embedder must make those effects safe.

`Runtime.End` cancels busy work, withdraws queued inputs, releases session connections, unregisters subscribers, and unloads the session. An owned `Stream` keeps accepted frames until drained or closed, so call `stream.Close()` when you do not consume it. It does not delete the append-only log or call a store cleanup callback. Cleanup continues if the caller's context times out, including when that context has ended before the call. If `End` returns a timeout, wait for the session's `Done` before deleting its log. The embedder owns that deletion. `Store`, `Owner`, hooks, and transports must honor their contexts; ignored cancellation in those dependencies can still delay cleanup.

## Stream text without loss

Use `Session.PromptStream` for an append-only consumer, such as the AI SDK.
It registers an independent lossless queue **before** it admits the input.
Reading `Stream.Events` can start after the model has streamed, or after the
whole turn has ended: all frames of that input's turn remain in order. The
queue has no size limit, so unread output uses memory until consumed or closed.
The actor and other subscribers never wait for this reader.

`PromptStream` uses queue delivery, even when `Input.Delivery` is empty. It
rejects steer delivery and typed control commands with `ErrInvalidRequest`.
Control commands are rejected before dispatch or recording; ordinary typed
prompts can still start model turns. If a prior turn runs, its
frames are not part of the new stream. Use a new input ID: a repeated identical
ID returns `ErrInvalidRequest`, because earlier ephemeral frames cannot be
recovered. A conflicting repeat still returns `ErrInputConflict`. Use the
original `Stream`, or the durable log, to read an earlier input.

Always `defer stream.Close()` after a successful call, even if you will not
iterate. `Close` is idempotent and safe for concurrent use. It releases queued
frames without interrupting the turn; use `stream.Cancel(ctx)` to stop the
input. Each stream permits one `Events` iteration. Completion, an early break,
or cancellation of either the prompt context or iterator context closes that
subscription. The context passed to `PromptStream` bounds the whole input, not just
admission. After admission, canceling it withdraws a queued input or interrupts
its running turn, including cancellation of the context passed to custom tools.
`Prompt` keeps its admission-only context behavior. A distinct context passed
to `Stream.Events` controls only that subscription. A canceled prompt context before the call admits no input. During admission,
an input can commit before the call returns a context error. The actor installs
the cancellation binding only for that new admission, before replying, so the
committed input is still canceled; a repeated earlier input is not. Use the
input ID to inspect the durable outcome. Keep the prompt context alive for the
life of the turn. The inherited `Wait` reads
the outcome without consuming or closing the lossless queue.

The prompt example uses this API. It prints every text delta without waiting
for completion. Delivery preserves `item.started`, all `item.delta` frames,
and `item.discarded` in order, before the durable record that follows them.
It does not synthesize deltas from completed messages. A retry can discard an
item after some text was streamed. An append-only display cannot retract that
text: preserve the item lifecycle in your UI, or mark discarded attempts rather
than showing their text as part of the final answer.

For an append-only client, also set `Options.DisableRetryAfterOutput: true`,
as the example does. Its default is false. In each harness-loop model call,
the first output delta disables recovery resends of **that call**, including
output-limit, truncated-stream, and context-overflow recovery. Text and reasoning
deltas count as output; partial tool-argument bytes do not. A failed call that
already streamed output
fails the turn rather than discarding that output and trying the call again.
Before its first output, a call keeps its normal retry budget. A later ordinary
model call starts with retries enabled, even if an earlier step produced text.
Provider pause/resume continuation remains allowed: it continues the response
rather than resending the failed call. The option does not change a delegated
backend's own retry policy. Cancellation still interrupts the turn. Partial
output already read from the stream remains available to the caller, but
`TurnResult` has no partial-text field; always check its stop reason and error.

### Reconcile previews and durable replay

Existing `Prompt`, `Turn.Events`, and `Session.Events` keep their bounded,
best-effort live delivery. A slow event consumer can fill its buffer. The
runtime then drops ephemeral frames without slowing the model. It never drops
durable records. `item.completed` contains the full message, including text
absent from a preview. Completed-item reconciliation is a backstop for these
streams and for reconnects, not the lossless streaming API.

Keep display state by `item_id`. Reset a preview on `item.started`, remove it
on `item.discarded`, and **replace**, not append, its text on `item.completed`.
Completion can arrive without a start or delta. Deltas can have gaps in the
middle, so appending a missing suffix is not a general repair.

This example uses `strings` in addition to the imports above. `items` represents
an application's display state. Render the changed entry after each event:

```go
items := map[string]string{}
var cursor uint64
for e, err := range s.Events(ctx, cursor) {
    if err != nil {
        log.Fatal(err)
    }
    switch e.Kind {
    case protocol.KindItemStarted, protocol.KindItemDelta, protocol.KindItemDiscarded:
        var f protocol.ItemFrame
        if err := json.Unmarshal(e.Data, &f); err != nil {
            log.Fatal(err)
        }
        switch e.Kind {
        case protocol.KindItemStarted:
            items[f.ItemID] = ""
        case protocol.KindItemDelta:
            if f.Type == "text" {
                items[f.ItemID] += f.Text
            }
        case protocol.KindItemDiscarded:
            delete(items, f.ItemID)
        }
    case "item.completed":
        var item protocol.ItemCompleted
        if err := json.Unmarshal(e.Data, &item); err != nil {
            log.Fatal(err)
        }
        var full strings.Builder
        for _, p := range item.Message.Parts {
            if p.Type == protocol.PartText {
                full.WriteString(p.Text)
            }
        }
        items[item.ItemID] = full.String()
    }
    if !e.Ephemeral {
        cursor = e.Seq
    }
}
```

Save the durable cursor **after** applying the record. Resume with
`s.Events(ctx, cursor)`. Never advance it from an ephemeral frame: its `Seq`
only places that preview after a durable record. To survive a process restart,
save completed item state and the cursor together, or rebuild them by replaying
from zero. `harness.ReadEvents(ctx, st, s.ID(), cursor)` reads only durable
records through the stored head; it does not follow a running turn. A replayed
completion replaces the same item by ID and does not duplicate its text.
The lossless guarantee covers completed items, not uncompleted previews.

## Per-session system blocks

Set `CreateSession.SystemBlocks` to replace the default system prompt for one session. A single string is a one-element list. Nil uses the runtime defaults; an empty list clears them. An empty string block is invalid. The runtime copies the list and stores it with the session, so `Open` restores it. Children use their own default prompt, not the parent's override. Tools, skills, and runtime notices remain available; MCP guidance follows the supplied blocks.

```go
breakpoint := 0
s, err := rt.Create(ctx, protocol.CreateSession{
    SystemBlocks: []string{"Stable application rules.", "Session-specific context."},
    SystemCacheBreakpoint: &breakpoint,
})
```

`SystemCacheBreakpoint` selects a zero-based system block for Anthropic's cache marker. The index must name a supplied block when an override is present, or a default session segment otherwise, before MCP guidance. Nil keeps the default: cache the last system block. The newest message block is cached by default. Set `config.Provider.CacheFinalMessage` to a pointer to false to omit that marker while keeping the selected system marker. `CacheTTL` selects `"5m"` or `"1h"` (default `"1h"`) for each remaining marker. Other provider wires ignore the selection. Blocks remain distinct through the model API path; providers that take a text prompt join them at their wire boundary. Claude Code uses the supplied text instead of the shared appended prompt, but keeps its own base prompt and runtime guidance. `SystemBlocks` and `SystemCacheBreakpoint` cannot change after creation.

For a five-minute system-only cache, set the provider entry in `Options.Config`:

```go
cacheFinalMessage := false
cfg := config.Config{
    Providers: map[string]config.Provider{
        "anthropic": {
            CacheTTL: "5m",
            CacheFinalMessage: &cacheFinalMessage,
        },
    },
}
```

Pass `cfg` as `Options.Config`. Keep the entry's credentials, auth, and endpoint settings when replacing an existing entry. Nil or a pointer to true keeps automatic final-message caching. This control applies to every model API call through that entry, not to Claude Code's own cache policy.

## Stop after a tool

Set `Options.StopAfterTools` to tool names that end a turn. If any named call succeeds, the runtime completes and records every call in that model step, then ends the turn without another model call. If all named calls fail, the model can try again. The option is off by default and applies to every session of the runtime. It has no effect on backends that own their loop. Set it again when opening a session on a new runtime.

Set `Options.StopAfter` when the result decides whether to stop. It is independent of `StopAfterTools`: either condition can end the turn. Leave a tool out of `StopAfterTools` when it should stop only if the predicate returns true.

```go
opts.StopAfter = func(call protocol.ToolCall, result protocol.ToolResult) bool {
    var output struct {
        Question string `json:"question"`
        Success  bool   `json:"success"`
    }
    if json.Unmarshal([]byte(result.Text), &output) != nil {
        return false
    }
    switch call.Name {
    case "suggestReplies":
        return output.Question != ""
    case "routeToService":
        return output.Success
    default:
        return false
    }
}
```

The predicate gets the original model call and the full successful result after `After` hooks, before `Join` hooks and retention. By default, it is not called for error results, tool errors, denied calls, calls that did not run, or an open question. Calls are serial in model call order within a turn, even after one returns true, but may overlap across sessions. Shared callback state must be concurrency-safe. Do not mutate the arguments, block, or panic; a panic is not recovered at this boundary. The predicate has no error return. Cancellation and recording errors still take precedence over a successful stop.

To inspect turn output as well, set `Options.StopAfterContext`. It receives the call, result, and a `StopContext` with the session and turn IDs, `TurnText`, and `ToolCalls` through the current result. `TurnText` joins recorded assistant text parts in order without a separator, across all model steps of this turn. `ToolCalls` holds completed calls and results, including failures. Ordinary results follow model call order; completed question results follow the ordinary results of their step. Its current entry has the full post-`After`, pre-`Join` result. Earlier entries use recorded results after `Join` and retention, without blob bytes; text may be truncated. The runtime rebuilds them from the log when a suspended turn resumes. The snapshot is independent of stream consumption and excludes pending questions and answers from other turns. Set `Options.StopAfterErrors` to call either predicate for failed ordinary results as well; the result contains the actual error. The default skips failures. Named-tool stops still require success. Both predicates run independently and follow the same callback rules.

For example, stop after `suggestReplies` even when it fails if the turn has already produced text:

```go
opts.StopAfterErrors = true
opts.StopAfterContext = func(call protocol.ToolCall, _ protocol.ToolResult, ctx harness.StopContext) bool {
    return call.Name == "suggestReplies" && ctx.TurnText != ""
}
```

Every call in the step finishes and its result is recorded before the turn completes. The stop takes precedence over `MaxSteps`; a waiting steer input runs in the next turn. The predicate is Go-only, off when nil, not persisted, and ignored by backends that own their loop. Supply it again when opening on a new runtime.

## Continue the session

Prompt the same session again with a new input ID:

```go
turn, err = s.PromptStream(ctx, protocol.Input{
	ID:    "prompt-2",
	Parts: []protocol.Part{{Type: protocol.PartText, Text: "Which of those files is the largest?"}},
})
```

Then close the new stream after reading its events and calling `turn.Wait`.
Use a new input ID for each `PromptStream`. For idempotent admission without a
lossless live stream, `Prompt` with the same ID and text returns the earlier
turn; the same ID with other text fails with `harness.ErrInputConflict`.

## Keep sessions across restarts

`store.NewMemStore` loses its sessions when the process exits. Use a disk store,
and open the session by its ID. Create it on the first run. Keep the store
outside `WorkDir`, or the model can read and change it:

```go
dir, err := os.UserCacheDir()
if err != nil {
	log.Fatal(err)
}
rt, err := harness.New(harness.Options{
	Store:   store.NewDiskStore(filepath.Join(dir, "my-app", "sessions")),
	WorkDir: ".",
	Getenv:  os.Getenv,
})
if err != nil {
	log.Fatal(err)
}
s, err := rt.Open(ctx, "my-session")
if errors.Is(err, harness.ErrSessionNotFound) {
	s, err = rt.Create(ctx, protocol.CreateSession{ID: "my-session"})
}
if err != nil {
	log.Fatal(err)
}
```

This adds the imports `errors` and `path/filepath`.

To update a recorded tool allowlist, pass `harness.WithAllowedTools(names)` to
`Open`. The runtime validates and records the replacement before any turn
resumes. A nil list allows every tool; an empty list allows none. Equal sets
record no change, even if their order or repeated names differ. `Snapshot`
returns the current list in `AllowedTools`.

Use `CreateSession.AllowedTools` on `Create`; `Create` rejects
`WithAllowedTools`. If `Create` returns `harness.ErrSessionExists`, call
`Open` with the option. An `Open` of a session that this runtime already
runs keeps its tools. No HTTP settings update can replace the allowlist.

## Give the model your own tools

A `harness.Tool` is a spec for the model and a Go function to run. This one
uses `math/rand/v2` and `strconv`:

```go
type rollDice struct{}

func (rollDice) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name:        "roll_dice",
		Description: "Roll a die with the given number of sides.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"sides": {"type": "integer", "minimum": 2}}, "required": ["sides"]}`),
	}
}

func (rollDice) Run(ctx context.Context, call harness.Call) (protocol.ToolResult, error) {
	var in struct{ Sides int }
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, err
	}
	if in.Sides < 2 {
		return protocol.ToolResult{}, fmt.Errorf("sides must be at least 2, got %d", in.Sides)
	}
	return protocol.ToolResult{Text: strconv.Itoa(rand.IntN(in.Sides) + 1)}, nil
}
```

Give it to the runtime:

```go
rt, err := harness.New(harness.Options{
	Store:   store.NewMemStore(),
	WorkDir: ".",
	Getenv:  os.Getenv,
	Tools:   []harness.Tool{rollDice{}},
})
```

## Run a plugin in the process

The same `Options` take plugins that run inside your program. See
[Run a plugin in the process](plugins.md#run-a-plugin-in-the-process).

## Serve over HTTP

`httpapi.Handler` serves the sessions of the runtime with the same API as
`harness serve`:

```go
log.Fatal(http.ListenAndServe("localhost:4096", httpapi.Handler(rt)))
```

The handler has no authentication. Add your own before you expose it.

See the [API reference](https://pkg.go.dev/github.com/majorcontext/harness) for
everything else.

## Embed in JavaScript

[`sdk/embed`](../sdk/embed/README.md) runs Harness as a WebAssembly module in a
Worker, or spawns `harness acp` under Node. Either way, you get an ACP stream.
In the WebAssembly module, the default tools have no `bash` or `process`;
`shellTool()` adds a shell for the browser. Keep model keys behind a
server, never in the page. For a native provider behind a credential-injecting
server, set `config.providers.anthropic.auth` (or `openai.auth`) to `"injected"`
and set `proxy.baseURL`. The URL alone does not supply credentials. A provider
entry with `auth: "injected"` sends no credential, and the server adds it;
`proxy.providers` sets the URLs of the server and of its sign-in page.

Injected auth also enables a status probe during ACP `session/new`, `session/load`,
and `session/resume`. The probe is a GET of the wire's model-list endpoint;
for Anthropic it is `<base_url>/v1/models` with `Anthropic-Version`.
A cross-origin probe can add a CORS preflight. A 401 probe rejects the session
request with `login_required`. Responses requests send a stable hash of the
session ID as `prompt_cache_key`, not the raw ID. An optional `login_url`
offers an interactive ACP auth method to clients with `elicitation.url`.
See the `sdk/embed` README for the server contract.

In the browser, an `mcp_servers` entry with a `url` works when the MCP server allows the origin of the page. To keep its credential out of the page, send it through a relay on your server, as [`examples/embed-server`](../examples/embed-server) does with the gatekeeper MCP relay.

## Run in Cloudflare Workers

A Cloudflare Worker cannot start a Worker. Use `wasm({worker: false})`, which
runs the module in the isolate of the Worker. The
[`sdk/embed` README](../sdk/embed/README.md#run-in-cloudflare-workers) has the
example and the constraints.

## Examples

[examples/](../examples) holds small programs that run. Clone the repository and
run them from its root:

```bash
git clone https://github.com/majorcontext/harness && cd harness
go run ./examples/prompt "Summarize README.md"
```

The examples set `Config.SubscriptionDefault`: they use `ANTHROPIC_API_KEY` when it is set, else the login of `harness login codex`, else the `claude` program on `PATH`.

- [`prompt`](../examples/prompt): send one prompt and stream the reply.
- [`custom-tool`](../examples/custom-tool): give the model a Go function to call.
- [`switch-model`](../examples/switch-model): continue one conversation on a
  different model or provider.
- [`guardrail`](../examples/guardrail): one Go plugin that blocks risky tool
  calls, in process and as a plugin process.
