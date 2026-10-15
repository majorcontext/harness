# Harness

A fast, extensible, composable agent harness in Go. Run it as a CLI, as an HTTP
server, as an ACP agent in your editor, or embedded in a Go or JavaScript
program.

[![CI](https://github.com/majorcontext/harness/actions/workflows/ci.yml/badge.svg)](https://github.com/majorcontext/harness/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/majorcontext/harness.svg)](https://pkg.go.dev/github.com/majorcontext/harness) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

- **Fast** — starts in tens of milliseconds
- **Extensible** — plugins in Go (in process or as a process) and in any language over stdio (TypeScript SDK included)
- **Composable** — headless runtime, event streams, client/server, MCP servers, editor integration over ACP, a WebAssembly build for the browser

## Before you start

- Go 1.27.1 or later, to install the CLI and to build Go plugins and programs.
- A ChatGPT account, or a Claude account with Claude Code
  installed. Harness signs in with the subscription, so no API key is needed.
  An API key also works, as [Choose a model](#choose-a-model) shows.
- A directory you can reset, such as a git repository. Harness has no approval
  prompts: the model reads and edits files and runs shell commands in the
  current directory without asking. Use a [plugin](docs/plugins.md) to block the
  calls you do not want.

## Quickstart

Install the CLI. `go install` puts it in `$(go env GOBIN)`, or in
`$(go env GOPATH)/bin` when `GOBIN` is empty. That directory must be on your
`PATH`:

```bash
go install github.com/majorcontext/harness/cmd/harness@latest
```

### Sign in

With a ChatGPT account, run `harness login codex`. It opens your browser; on a
machine with no browser, run `harness login codex --device` and enter the code
it prints on another device. With a Claude account, log in to Claude Code with
`claude auth login` or `harness login claude-code`. `harness login codex` and
`harness login claude-code` write `model` to your user config when it has none.
See [Configuration](docs/configuration.md) for both logins.

### Run a prompt

Run a prompt in your project:

```bash
harness run -p "Find the TODOs in this repo and fix the easy ones"
```

With no model set, Harness picks one from your API key and logins; see
[Default model](docs/configuration.md#default-model).

The reply goes to standard output. The tool calls, the session ID, and JSON log
lines go to standard error. Run `harness --help` for every `run` flag.

### Choose a model

Model refs are `provider/model`. `-model` picks the model for one run:

```bash
harness run -model codex/gpt-6.1-sol -p "Find the TODOs in this repo"
harness run -model claude-code/sonnet -p "Find the TODOs in this repo"
```

To set the default yourself, put `model` in a config file:
`{"model": "codex/gpt-6.1-sol"}`. To pay by API key, set `ANTHROPIC_API_KEY` or
`OPENAI_API_KEY` and use an `anthropic/` or `openai/` model, such as
`openai/gpt-5-mini`. [Configuration](docs/configuration.md) shows how to add
OpenRouter, Ollama, and other endpoints.

### Continue a session

`-c` continues the newest session that you started in the current directory. It
fails when that directory has none. `harness sessions` lists the saved sessions
(ID, creation time, message count), and `-r <id>` resumes one:

```bash
harness run -c -p "Now write a test for each fix"
```

### Config files

Harness reads two optional JSON files: `~/.harness/config.json` for every
project (`$HARNESS_CONFIG` names another file), and `.harness.json` in the
project directory, which overlays it. See `config.Config` for every field.

A `.harness.json` is code. Harness runs the programs that it names
(`mcp_servers`, `plugins`, `processes`, and `binary_path` of a `claude-code-cli`
provider) and connects to the URLs that it names, with no prompt. Do not run
Harness in a checkout that you do not trust. An embedder that opens untrusted
directories must pass its own config and isolate the process. The
[architecture spec](docs/architecture.md#config) lists every key.

### Go further

- [Give the model more tools](docs/cli.md#give-the-model-more-tools) with an MCP server.
- [Work toward a goal](docs/cli.md#work-toward-a-goal) until an evaluator model judges it met.
- [Block tool calls with a plugin](docs/plugins.md).
- [Serve it over HTTP](docs/cli.md#serve-it-over-http) with `harness serve`.
- [Use it in your editor](docs/cli.md#use-harness-in-your-editor) over ACP.

## Embed Harness in Go

The runtime is a Go package. The CLI and the HTTP server are clients of it:

```go
rt, err := harness.New(harness.Options{
	Store:   store.NewMemStore(),
	WorkDir: ".",
	Getenv:  os.Getenv,
	Config:  config.Config{Model: "claude-code/sonnet"},
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
```

Read the reply from `turn.Events` and call `turn.Wait`; [Embedding](docs/embedding.md)
has the complete program, the Codex login (`Config.CodexAuthFile`), sessions
that survive a restart, your own tools, and plugins in the process. The
[API reference](https://pkg.go.dev/github.com/majorcontext/harness) lists
everything else.

## Embed Harness in JavaScript

[`sdk/embed`](sdk/embed/README.md) runs Harness as a WebAssembly module in a
Worker, or spawns `harness acp` under Node, and gives you an ACP stream. Keep
model keys behind a same-origin proxy, never in the page. See
[Embedding](docs/embedding.md#embed-in-javascript).

## Examples

[examples/](examples) holds small programs that run (`prompt`, `custom-tool`,
`switch-model`, `guardrail`). They need `ANTHROPIC_API_KEY`; see
[Embedding](docs/embedding.md#examples).

## Configuration

Three kinds of provider serve a model ref. `codex` and `claude-code` are built
in and use your subscription login, with no config. `anthropic` and `openai` are
built in and read an API key from the environment. A `providers` entry adds an
OpenAI-compatible endpoint (Ollama, vLLM, OpenRouter) or an OpenAI Responses
endpoint. `web` turns on the opt-in `web_fetch` tool (`{"fetch": true}`) and the
web search of the provider (`{"search": true}`). A proxy that holds the
credential can serve a provider with `auth: "injected"`; see
[Let a proxy add the credential](docs/configuration.md#let-a-proxy-add-the-credential).
See [Configuration](docs/configuration.md).

## Contributing

Read [AGENTS.md](AGENTS.md) first. It holds the repository-wide rules. Read the
`AGENTS.md` in each directory you change. [docs/README.md](docs/README.md) indexes the
technical documentation.

---

Part of [Major Context](https://majorcontext.com).

[Moat](https://github.com/majorcontext/moat) · [Keep](https://github.com/majorcontext/keep) · [Gatekeeper](https://github.com/majorcontext/gatekeeper) · Bailey · **Harness**

MIT licensed. See [LICENSE](LICENSE).
