# Harness

A fast, extensible, composable agent harness in Go.

[![CI](https://github.com/majorcontext/harness/actions/workflows/ci.yml/badge.svg)](https://github.com/majorcontext/harness/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/majorcontext/harness.svg)](https://pkg.go.dev/github.com/majorcontext/harness) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

- **Fast** — millisecond startup
- **Extensible** — language-agnostic process plugins (TypeScript SDK included)
- **Composable** — headless runtime, event streams, client/server, MCP servers

## Quickstart

Install the CLI:

```bash
go install github.com/majorcontext/harness/cmd/harness@latest
```

### Run a prompt

The default model is Anthropic's. Set its key, then run a prompt in your project:

```bash
export ANTHROPIC_API_KEY=...
harness run -p "Find the TODOs in this repo and fix the easy ones"
```

Harness reads files, edits them, and runs commands in the current directory.
It saves each session.

### Continue the session

`-c` continues the most recent session, so the model knows what it did:

```bash
harness run -c -p "Now write a test for each fix"
```

### Go back to an older session

`harness sessions` lists the saved sessions, oldest first. Each line has the ID,
the creation time, and the message count:

```console
$ harness sessions
ses_ctqgq5qegmlzj2xub2rq5qewqj	2026-10-12T19:11:50Z	10
ses_fplt2uzhgiwsj6celuziopq7nl	2026-10-12T19:12:09Z	2
```

`-r` resumes a session by its ID:

```bash
harness run -r ses_ctqgq5qegmlzj2xub2rq5qewqj -p "What did we change?"
```

Sessions are in `~/.harness/sessions`. `harness sessions --json` prints the
same list as JSON.

### Give the model more tools

Add an MCP server to `.harness.json` in your project. This one lets the model
fetch web pages. It needs [uv](https://docs.astral.sh/uv/).

```json
{
  "mcp_servers": {
    "fetch": {"command": ["uvx", "mcp-server-fetch"]}
  }
}
```

```bash
harness run -p "Fetch https://go.dev/VERSION?m=text and tell me the latest Go release"
```

For a remote server, set `url` and `headers` in place of `command`.

### Work toward a goal

`-goal` runs turns until an independent evaluator model judges the condition
met. Name the evaluator in `.harness.json`:

```json
{
  "goal_evaluator_model": "anthropic/claude-fable-5",
  "mcp_servers": {
    "fetch": {"command": ["uvx", "mcp-server-fetch"]}
  }
}
```

```bash
harness run -goal "go test ./... passes"
```

The command exits 0 when the goal is met, 3 when it is not, and 1 when a turn
fails.

### Serve it over HTTP

```bash
harness serve
```

In another terminal, create a session, send it a prompt, and stream its events:

```bash
curl -X POST localhost:4096/sessions -d '{"id": "demo"}'
curl -X POST localhost:4096/sessions/demo/inputs \
  -d '{"id": "1", "parts": [{"type": "text", "text": "Say hello"}]}'
curl -N -H 'Accept: text/event-stream' localhost:4096/sessions/demo/events
```

With no `HARNESS_RUN_TOKEN` set, the server needs no token on `localhost`.
Run `harness --help` for the commands and the `run` flags, and
`harness serve -h` for the `serve` flags.

## Embed Harness in Go

The runtime is a Go package. The CLI and the HTTP server are clients of it.
Add it to your module:

```bash
go get github.com/majorcontext/harness@latest
```

### Run a prompt

Create a runtime and a session, send the session a prompt, and read the events
of its turn:

```go
ctx := context.Background()
rt, err := harness.New(harness.Options{
	Store:   store.NewMemStore(),
	WorkDir: ".",
	Getenv:  os.Getenv,
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
res, err := turn.Wait(ctx)
if err != nil {
	log.Fatal(err)
}
if res.StopReason == protocol.StopFailed {
	log.Fatal(res.Error)
}
```

`Getenv` lets the runtime read the API-key variables of the model providers.
Without it, a provider has no key unless `ModelTransport` supplies the
credentials. `WorkDir` gives each session the built-in tools, bash among them.
Leave it empty for a session with no file access. A failed turn does not end
`Events` with an error, so check `turn.Wait`.

### Continue the session

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

### Keep sessions across restarts

`store.NewMemStore` loses its sessions when the process exits. Use a disk store,
and open the session by its ID. Create it on the first run:

```go
rt, err := harness.New(harness.Options{
	Store:   store.NewDiskStore("sessions"),
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

### Give the model your own tools

A `harness.Tool` is a spec for the model and a Go function to run:

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
	Store:   store.NewDiskStore("sessions"),
	WorkDir: ".",
	Getenv:  os.Getenv,
	Tools:   []harness.Tool{rollDice{}},
})
```

### Serve over HTTP

`httpapi.Handler` serves the sessions of the runtime with the same API as
`harness serve`:

```go
log.Fatal(http.ListenAndServe("localhost:4096", httpapi.Handler(rt)))
```

The handler has no authentication. Add your own before you expose it.

See the [API reference](https://pkg.go.dev/github.com/majorcontext/harness) for
everything else.

## Examples

[examples/](examples) holds small programs that run:

- [`prompt`](examples/prompt): send one prompt and stream the reply.
- [`custom-tool`](examples/custom-tool): give the model a Go function to call.
- [`switch-model`](examples/switch-model): continue one conversation on a
  different model or provider.

## Configuration

Config lives at `~/.harness/config.json`. Set `$HARNESS_CONFIG` to use another
file. A per-project `.harness.json` overlays it. See `config.Config` for every
field.

Model refs are `provider/model`. `provider` is a built-in family (`anthropic`,
`openai`, `openrouter`) or a key from `providers`.

### OpenAI-compatible endpoints

Ollama, vLLM, LM Studio, and other chat-completions endpoints take one
`providers` entry. The adapter sends a key on every request, so set one even
if your server does not check it:

```bash
export OLLAMA_API_KEY=ollama
```

Start Ollama with the context window Harness will assume:

```bash
OLLAMA_CONTEXT_LENGTH=131072 ollama serve
```

Quit the Ollama app first. It serves port 11434 without this setting.

Then add the entry:

```json
{
  "context_window_tokens": 131072,
  "providers": {
    "ollama": {
      "type": "openai-compat",
      "base_url": "http://localhost:11434/v1",
      "api_key_env": "OLLAMA_API_KEY"
    }
  }
}
```

Keep `context_window_tokens` equal to the server's window, not the model's
maximum. If the server's window is smaller, it drops context before Harness
compacts.

The key becomes the provider name, so the model ref is `ollama/llama3.1`.
Optional fields: `family` (the wire-quirk tag, defaults to the key) and
`extra_headers` (sent on every request).

### OpenRouter

Harness registers an `openrouter` provider when `providers` has no
`openrouter` entry, so you only need a key:

```bash
export OPENROUTER_API_KEY=...
harness run -model openrouter/anthropic/claude-sonnet-5 -p "Review the diff"
```

An `openrouter` entry takes `type`, `base_url`, and `api_key_env` from those
defaults when you leave them out. To send attribution headers, set only
`extra_headers`:

```json
{
  "providers": {
    "openrouter": {
      "extra_headers": {"HTTP-Referer": "https://example.com", "X-Title": "my-app"}
    }
  }
}
```

### OpenAI Responses endpoints

An endpoint that speaks the OpenAI Responses API uses `type: "openai"`, under
any key. `responses_path` points it at an endpoint that does not serve
`/v1/responses`:

```json
{
  "context_window_tokens": 128000,
  "providers": {
    "vendor": {
      "type": "openai",
      "base_url": "https://api.vendor.example",
      "api_key_env": "VENDOR_API_KEY",
      "responses_path": "/backend/responses"
    }
  }
}
```

The model ref `vendor/some-model` sends `some-model` as the model ID. Set
`context_window_tokens` to your model's context window.
`responses_path` is also valid on the built-in `openai` entry, and nowhere
else.

### Context windows

Automatic compaction needs the context window of the model. A model that is
missing from the built-in context-window catalog, such as an OpenRouter or
local model, runs with a window of 128000 tokens. Harness logs one warning for
the model, and the session gauge sets `window_estimated`. When the provider
rejects a request as too long, Harness compacts and tries again; it does not
learn the window from the error. To name the window yourself, set
`context_window_tokens`. It applies to every session of a model API provider
and overrides the catalog.

### Config errors

These fail config loading with an error that names the entry: an unknown
`type`, an `openai-compat` or `openai` entry with no `base_url` (except
`openrouter`, which has a default), and a misplaced `responses_path`.

## Contributing

Read [AGENTS.md](AGENTS.md) first. It holds the repository-wide rules. Read the
`AGENTS.md` in each directory you change. [docs/README.md](docs/README.md) indexes the
technical documentation.

---

Part of [Major Context](https://majorcontext.com).

[Moat](https://github.com/majorcontext/moat) · [Keep](https://github.com/majorcontext/keep) · [Gatekeeper](https://github.com/majorcontext/gatekeeper) · Bailey · **Harness**

MIT licensed. See [LICENSE](LICENSE).
