# harness

A fast, extensible, composable agent harness in Go.

- **Fast** — millisecond startup, CI-enforced budgets
- **Extensible** — language-agnostic process plugins with a Go SDK
- **Composable** — headless engine, event streams, client/server, MCP both directions
- **Model-fluid** — swap providers/models mid-session or per-subagent with no migration

See [AGENTS.md](AGENTS.md) for architecture and design decisions.

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
