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
	turn, err := s.Prompt(ctx, protocol.Input{
		ID:    "prompt-1",
		Parts: []protocol.Part{{Type: protocol.PartText, Text: "List the files in this directory."}},
	})
	if err != nil {
		log.Fatal(err)
	}
	for e, err := range turn.Events(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		if e.Kind == protocol.KindItemDelta {
			var f protocol.ItemFrame
			if json.Unmarshal(e.Data, &f) == nil && f.Type == "text" {
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

## Continue the session

Prompt the same session again with a new input ID:

```go
turn, err = s.Prompt(ctx, protocol.Input{
	ID:    "prompt-2",
	Parts: []protocol.Part{{Type: protocol.PartText, Text: "Which of those files is the largest?"}},
})
```

Then read its events and call `turn.Wait` as before. The input ID makes a retry
safe: the same ID and text returns the earlier turn, and the same ID with other
text fails with `harness.ErrInputConflict`.

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
