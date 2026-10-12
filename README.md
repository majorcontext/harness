# Harness

A fast, extensible, composable agent harness in Go.

[![CI](https://github.com/majorcontext/harness/actions/workflows/ci.yml/badge.svg)](https://github.com/majorcontext/harness/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/majorcontext/harness.svg)](https://pkg.go.dev/github.com/majorcontext/harness) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

- **Fast** — millisecond startup, CI-enforced budgets
- **Extensible** — language-agnostic process plugins (TypeScript SDK included)
- **Composable** — headless runtime, event streams, client/server, MCP servers

## Install

```bash
go install github.com/majorcontext/harness/cmd/harness@latest
```

## Get started

The default model is Anthropic's. Set its key:

```bash
export ANTHROPIC_API_KEY=...
```

Run a prompt in your project:

```bash
harness run -p "Find the TODOs in this repo and fix the easy ones"
```

Continue the most recent session:

```bash
harness run -c -p "Now write a test for each fix"
```

Use another provider's model. Set that provider's key first, for example `OPENAI_API_KEY`:

```bash
harness run -model openai/gpt-5 -p "Review the diff"
```

Pursue a goal until an independent evaluator judges it met. This needs `goal_evaluator_model` in your config:

```bash
harness run -goal "go test ./... passes"
```

Serve the HTTP+SSE session API on `localhost:4096`:

```bash
harness serve
```

Run `harness --help` for all commands and flags. `harness sessions` lists saved sessions, and `harness plugin probe` probes the configured plugins and prints their hooks.

## Use the library

The runtime is a Go package. The CLI and the HTTP server are clients of it. Add it to your module:

```bash
go get github.com/majorcontext/harness@latest
```

Then create a runtime and a session, send the session a prompt, and read the events of its turn:

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

`Getenv` lets the runtime read provider API keys. Without it, no provider has a key.
`WorkDir` gives each session the built-in tools, bash among them. Leave it empty for a session with no file access.
A failed turn does not end `Events` with an error. `turn.Wait` returns its stop reason, error, and usage.
`httpapi.Handler(rt)` serves the same sessions over HTTP, and `store.NewDiskStore` keeps them on disk.

See [examples/](examples) for programs that run, and the
[API reference](https://pkg.go.dev/github.com/majorcontext/harness) for
everything else.

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
`context_window_tokens`. It applies to every session and overrides the catalog.

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
