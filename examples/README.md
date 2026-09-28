# Examples

Each directory is a small program. Run it from the repository root.

| Example | What it shows |
|---|---|
| [`prompt`](prompt) | Send one prompt and stream the reply. The model can use the built-in tools. |
| [`custom-tool`](custom-tool) | Give the model a Go function to call. |
| [`switch-model`](switch-model) | Continue one conversation on a different model or provider. |
| [`plugins/redactor.mjs`](plugins/redactor.mjs) | A process plugin written with the TypeScript SDK. |

```bash
ANTHROPIC_API_KEY=... go run ./examples/prompt "Summarize README.md"
```

The engine writes diagnostic logs to stderr. Set `Config.OnTurnMetrics` and
the default `slog` logger to silence them.
