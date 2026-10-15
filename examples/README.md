# Examples

Each directory is a small program. Run it from the repository root.

| Example | What it shows |
|---|---|
| [`prompt`](prompt) | Send one prompt and stream the reply. The model can use the built-in tools. |
| [`custom-tool`](custom-tool) | Give the model a Go function to call. |
| [`switch-model`](switch-model) | Continue one conversation on a different model or provider. |
| [`guardrail`](guardrail) | One Go plugin value that blocks a force push, `rm -rf` on a broad target, and a `read_file` call on a `.env` file. It runs in the host process and as a process plugin. |
| [`embed-server`](embed-server) | A server for the WebAssembly module in a page: it holds the ChatGPT login of each user and adds it to the model requests. Its own Go module. |
| [`plugins/redactor.mjs`](plugins/redactor.mjs) | A process plugin written with the TypeScript SDK. |

```bash
go run ./examples/prompt "Summarize README.md"
```

Each example other than `embed-server` sets `Config.SubscriptionDefault` and
takes the default model of your machine, the same order as `harness run`:

1. With `ANTHROPIC_API_KEY` set, the API default.
2. Else a ChatGPT login from `harness login codex` selects `codex/gpt-6.1-sol`.
3. Else the `claude` program on `PATH` selects `claude-code/sonnet`. The CLI
   keeps its own login; run `harness login claude-code` to sign in.

`examples/internal/login` sets `Config.CodexAuthFile` to `auth/codex.json`
beside `$HARNESS_CONFIG`, else in `~/.harness`; the Go API has no default for
it. `switch-model` continues on another model that can run. Its `-first` and
`-then` flags take any `provider/model`.

The `guardrail` example runs in the host process with `harness.Options.Plugins`.
To run the same plugin as a process, build `guardrail-plugin` and add it to
the `plugins` key of a config file:

```bash
go build -o /usr/local/bin/guardrail-plugin ./examples/guardrail/cmd/guardrail-plugin
```

```json
{"plugins": [{"name": "guardrail", "command": ["guardrail-plugin"]}]}
```

The guard reads the text of a command. It is not a sandbox.

The runtime writes diagnostic logs to stderr through the default `slog`
logger. Call `slog.SetDefault` to redirect them.
