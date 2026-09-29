# Harness

A fast, extensible, composable agent harness in Go.

[![CI](https://github.com/majorcontext/harness/actions/workflows/ci.yml/badge.svg)](https://github.com/majorcontext/harness/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/majorcontext/harness.svg)](https://pkg.go.dev/github.com/majorcontext/harness) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

- **Fast** — millisecond startup, CI-enforced budgets
- **Extensible** — language-agnostic process plugins with a Go SDK
- **Composable** — headless engine, event streams, client/server, MCP both directions
- **Model-fluid** — swap providers/models mid-session or per-subagent with no migration

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

Run `harness --help` for all commands and flags.

## Use the library

The engine is a Go package. The CLI and server are clients of it. Add it to your module:

```bash
go get github.com/majorcontext/harness/engine@latest
```

Then create a session and send it a prompt:

```go
s := engine.NewSession(engine.Config{
	Providers: provider.Registry{
		anthropic.Family: &anthropic.Client{APIKey: os.Getenv("ANTHROPIC_API_KEY")},
	},
	Model:   message.ModelRef{Provider: anthropic.Family, Model: "claude-fable-5"},
	WorkDir: ".",
	OnEvent: func(e engine.Event) {
		if e.Type == engine.EventTextDelta {
			fmt.Print(e.Text)
		}
	},
})
if _, err := s.Prompt(context.Background(), "List the files in this directory."); err != nil {
	log.Fatal(err)
}
```

See [examples/](examples) for programs that run, and the
[API reference](https://pkg.go.dev/github.com/majorcontext/harness/engine) for
everything else.

## Configuration

Config lives at `~/.harness/config.json`. Set `$HARNESS_CONFIG` to use another
file. A per-project `.harness.json` overlays it. See `config.Config` for every
field.

Model refs are `provider/model`. `provider` is a built-in family (`anthropic`,
`openai`, `openrouter`) or a key from `providers`.

Harness refuses to run a model whose context window it does not know, because
automatic compaction needs that size. Models outside its built-in catalog,
including every OpenRouter and local model, need `context_window_tokens`. It
applies to every session.

### OpenAI-compatible endpoints

Ollama, vLLM, LM Studio, and other chat-completions endpoints take one
`providers` entry:

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

The key becomes the provider name, so the model ref is `ollama/llama3.1`.
Optional fields: `family` (the wire-quirk tag, defaults to the key) and
`extra_headers` (sent on every request).

### OpenRouter

Harness registers an `openrouter` provider when `providers` has none, so you
only need a key and a context window:

```bash
export OPENROUTER_API_KEY=...
```

```json
{
  "context_window_tokens": 200000
}
```

```bash
harness run -model openrouter/anthropic/claude-sonnet-5 -p "Review the diff"
```

An `openrouter` entry in `providers` replaces the built-in one entirely. Use
one to send attribution headers:

```json
{
  "providers": {
    "openrouter": {
      "type": "openai-compat",
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_API_KEY",
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

The model ref `vendor/some-model` sends `some-model` as the model ID.
`responses_path` is also valid on the built-in `openai` entry, and nowhere
else.

An unknown `type`, a missing `base_url`, or a misplaced `responses_path` fails
config loading with an error that names the entry.

## Contributing

Read [AGENTS.md](AGENTS.md) first. It holds repository-wide rules and an index
of each subsystem's `AGENTS.md`. [docs/README.md](docs/README.md) indexes the
technical documentation.

---

Part of [Major Context](https://majorcontext.com).

[Moat](https://github.com/majorcontext/moat) · [Keep](https://github.com/majorcontext/keep) · [Gatekeeper](https://github.com/majorcontext/gatekeeper) · Bailey · **Harness**

MIT licensed. See [LICENSE](LICENSE).
