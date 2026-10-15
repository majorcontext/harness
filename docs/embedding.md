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
the API-key variables; without it, a provider has no key unless
`ModelTransport` supplies the credentials. The later snippets show `Options`
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
same-origin proxy, never in the page. The module can also sign in with a
ChatGPT subscription by the device code flow when the page is the user's own;
see the `sdk/embed` README.

## Examples

[examples/](../examples) holds small programs that run. Clone the repository and
run them from its root:

```bash
git clone https://github.com/majorcontext/harness && cd harness
ANTHROPIC_API_KEY=... go run ./examples/prompt "Summarize README.md"
```

The examples use an API key: set `ANTHROPIC_API_KEY`.

- [`prompt`](../examples/prompt): send one prompt and stream the reply.
- [`custom-tool`](../examples/custom-tool): give the model a Go function to call.
- [`switch-model`](../examples/switch-model): continue one conversation on a
  different model or provider.
- [`guardrail`](../examples/guardrail): one Go plugin that blocks risky tool
  calls, in process and as a plugin process.
