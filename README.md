# Harness

```text
██╗  ██╗ █████╗ ██████╗ ███╗   ██╗███████╗███████╗███████╗
██║  ██║██╔══██╗██╔══██╗████╗  ██║██╔════╝██╔════╝██╔════╝
███████║███████║██████╔╝██╔██╗ ██║█████╗  ███████╗███████╗
██╔══██║██╔══██║██╔══██╗██║╚██╗██║██╔══╝  ╚════██║╚════██║
██║  ██║██║  ██║██║  ██║██║ ╚████║███████╗███████║███████║
╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═══╝╚══════╝╚══════╝╚══════╝
```

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

## Use the CLI

```bash
export ANTHROPIC_API_KEY=...
harness run -p "Find the TODOs in this repo and fix the easy ones"
harness run -c -p "Now write a test for each fix"                        # continue the last session
OPENAI_API_KEY=... harness run -model openai/gpt-5 -p "Review the diff"  # any provider/model
harness run -goal "go test ./... passes"                                 # needs goal_evaluator_model in config
harness serve                                                            # HTTP+SSE session API
```

Run `harness --help` for all commands and flags.

## Use the library

The engine is a Go package. The CLI and server are clients of it.

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

Config lives at `~/.harness/config.json` (override with `$HARNESS_CONFIG`),
optionally overlaid by a per-project `.harness.json`. It's a flat JSON file;
see `config.Config` for the full field list. Model refs are `provider/model`,
and `provider` is either a built-in family (`anthropic`, `openai`) or a name
from `providers`.

Any OpenAI-compatible chat-completions endpoint — OpenRouter, Ollama, vLLM,
LM Studio, and the like — is a two-line `providers` entry, no code required:

```json
{
  "providers": {
    "ollama": {
      "type": "openai-compat",
      "base_url": "http://localhost:11434/v1",
      "api_key_env": "OLLAMA_API_KEY"
    }
  }
}
```

The map key becomes the provider name for model refs, e.g.
`"model": "ollama/llama3.1"`. Optional fields: `family` (the wire-quirk /
`ProviderData` tag, defaults to the map key) and `extra_headers` (sent
verbatim on every request, e.g. OpenRouter's attribution headers):

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

OpenRouter itself needs *no* config at all: if `providers` has no
`openrouter` entry, harness registers one automatically with the base URL
and `api_key_env` above, so `"model": "openrouter/anthropic/claude-sonnet-5"`
works as soon as `OPENROUTER_API_KEY` is set. Any `openrouter` entry in
config — even a partial one — overrides the built-in default entirely.

An endpoint that speaks the OpenAI **Responses** API rather than
chat-completions uses `type: "openai"`, which builds the same native adapter
the built-in `openai` family uses. It works under any map key, so a second
Responses endpoint can sit beside the built-in one, and `responses_path`
points it at an endpoint that does not serve `/v1/responses`:

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

`"model": "vendor/some-model"` then routes there, passing `some-model`
through as the model id. `responses_path` defaults to `/v1/responses` and is
also accepted on the built-in `openai` entry; it is rejected on any other
kind of entry, since no other adapter reads it.

An unrecognized `type`, an `openai-compat` or `openai` entry missing
`base_url`, or a `responses_path` on an entry that builds neither Responses
adapter, fails config loading loudly rather than silently registering
nothing.

## Contributing

Read [AGENTS.md](AGENTS.md) first. It holds repository-wide rules and an index
of each subsystem's `AGENTS.md`. [docs/README.md](docs/README.md) indexes the
technical documentation.

---

Part of [Major Context](https://majorcontext.com).

[Moat](https://github.com/majorcontext/moat) · [Keep](https://github.com/majorcontext/keep) · [Gatekeeper](https://github.com/majorcontext/gatekeeper) · [Bailey](https://github.com/majorcontext/bailey) · **Harness**

MIT licensed. See [LICENSE](LICENSE).
