# Harness

A fast, extensible, composable agent harness in Go.

[![CI](https://github.com/majorcontext/harness/actions/workflows/ci.yml/badge.svg)](https://github.com/majorcontext/harness/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/majorcontext/harness.svg)](https://pkg.go.dev/github.com/majorcontext/harness) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

- **Fast** — starts in tens of milliseconds
- **Extensible** — plugins in Go (in process or as a process) and in any language over stdio (TypeScript SDK included)
- **Composable** — headless runtime, event streams, client/server, MCP servers

## Before you start

- Go 1.27.1 or later, to install the CLI and to build Go plugins and programs.
- An API key for one model provider. The default model is Anthropic's
  (`ANTHROPIC_API_KEY`). With an OpenAI key, choose an OpenAI model, as
  [Choose a model](#choose-a-model) shows. For any other provider, add a
  `providers` entry, as [Configuration](#configuration) shows.
- A directory you can reset, such as a git repository. Harness has no approval
  prompts: the model reads and edits files and runs shell commands in the
  current directory without asking. Use a [plugin](#block-tool-calls-with-a-plugin)
  to block the calls you do not want.

## Quickstart

Install the CLI. `go install` puts it in `$(go env GOBIN)`, or in
`$(go env GOPATH)/bin` when `GOBIN` is empty. That directory must be on your
`PATH`:

```bash
go install github.com/majorcontext/harness/cmd/harness@latest
```

### Run a prompt

Set the key, then run a prompt in your project:

```bash
export ANTHROPIC_API_KEY=...
harness run -p "Find the TODOs in this repo and fix the easy ones"
```

The reply goes to standard output. The tool calls, the session ID, and JSON log
lines go to standard error. Run `harness --help` for every `run` flag.

### Choose a model

Model refs are `provider/model`. `-model` picks the model for one run:

```bash
export OPENAI_API_KEY=...
harness run -model openai/gpt-5-mini -p "Find the TODOs in this repo"
```

To change the default, set `model` in a config file (see
[Config files](#config-files)):

```json
{"model": "openai/gpt-5-mini"}
```

[Configuration](#configuration) shows how to add OpenRouter, Ollama, and other endpoints.

### Continue the session

`-c` continues the newest session that you started in the current directory, so the model knows what it did. A session from another directory is never chosen, and `-c` fails when the current directory has no session:

```bash
harness run -c -p "Now write a test for each fix"
```

`-c` continues the newest session that you started in the current directory.
It fails when that directory has none, and never continues a session of another
project. Use `-r` for any other session.

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

### Config files

Harness reads two JSON files, and both are optional:

- `~/.harness/config.json` holds your settings for every project. Create it
  when you need it. Set `$HARNESS_CONFIG` to use another file.
- `.harness.json` in the project directory overlays it.

See `config.Config` for every field. The next steps add keys to
`.harness.json`.

A `.harness.json` is code. Harness runs the programs that it names (`mcp_servers`,
`plugins`, `processes`, and `binary_path` of a `claude-code-cli` provider) and
connects to the URLs that it names, with no prompt. Do not run Harness in a
checkout that you do not trust. An embedder that opens untrusted directories
must pass its own config and isolate the process. The
[architecture spec](docs/architecture.md#config) lists every key.

### Give the model more tools

This MCP server lets the model fetch web pages. It needs
[uv](https://docs.astral.sh/uv/).

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
met. Add the evaluator to `.harness.json`. It needs the key of its own
provider:

```json
{"goal_evaluator_model": "anthropic/claude-fable-5"}
```

```bash
harness run -goal "go test ./... passes"
```

The command exits 0 when the goal is met, 3 when the evaluator judges it not met, and 1 when a turn
or the goal evaluator fails.

### Block tool calls with a plugin

A plugin runs before each tool call and can deny it. This one is a Go program
that blocks `rm` in shell commands. Make a module for it:

```bash
mkdir guard-plugin && cd guard-plugin
go mod init example.com/guard-plugin
go get github.com/majorcontext/harness@latest
```

Write `main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/majorcontext/harness/plugin"
)

var guard = &plugin.Plugin{
	Name: "guard",
	ToolExecuteBefore: &plugin.ToolBefore{
		DenyOnFailure: true,
		Run:           check,
	},
}

func check(ctx context.Context, c *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
	if req.Tool != "bash" {
		return nil, nil
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(req.Args, &args); err != nil {
		return nil, err
	}
	for _, word := range strings.Fields(args.Command) {
		if word == "rm" {
			return &plugin.ToolExecuteBeforeResponse{Deny: "rm is blocked"}, nil
		}
	}
	return nil, nil
}

func main() {
	if err := plugin.Serve(guard); err != nil {
		log.Fatal(err)
	}
}
```

`Run` returns nil to let the call run, or a `Deny` message, which the model
gets as the error result of the call. With `DenyOnFailure`, a hook that
returns an error, panics, or misses its deadline also denies the call, so a
broken guard does not let a call through. `plugin.Serve` speaks the protocol
on standard input and output, so the plugin must log only to standard error.

Install it. The binary takes the last element of the module path,
`guard-plugin`:

```bash
go install .
```

Add it to the `plugins` key of `.harness.json` in your project. `name` must
equal `Plugin.Name`, and `command` is found on your `PATH`:

```json
{"plugins": [{"name": "guard", "command": ["guard-plugin"]}]}
```

Check that Harness finds it. The probe prints each plugin and its hooks:

```console
$ harness plugin probe
guard: tool.execute.before
```

Then ask for a call it blocks. The tool lines show the call and the deny
message:

```console
$ echo scratch > scratch.txt
$ harness run -p "Delete scratch.txt with the bash tool"
[tool bash] {"command":"rm scratch.txt"}
[tool bash failed] rm is blocked
```

The guard reads the text of a command. It is not a sandbox: the model can try
another command to get the same result, such as a Python script that deletes
the file. After a rebuild, the next run or probe sees the new binary. A plugin can also
change requests and results, add tools, and call back into the host. A plugin
in another language speaks the same protocol over stdio. See the
[`plugin` package reference](https://pkg.go.dev/github.com/majorcontext/harness/plugin),
the [TypeScript SDK](sdk/typescript/README.md), the
[protocol](internal/plugin/PROTOCOL.md), and
[`examples/guardrail`](examples/guardrail).

### Serve it over HTTP

```bash
harness serve
```

In another terminal, create a session, send it a prompt, and stream its events.
The last command streams until you stop it with Ctrl-C:

```bash
curl -X POST localhost:4096/sessions -d '{"id": "demo"}'
curl -X POST localhost:4096/sessions/demo/inputs \
  -d '{"id": "1", "parts": [{"type": "text", "text": "Say hello"}]}'
curl -N -H 'Accept: text/event-stream' localhost:4096/sessions/demo/events
```

To choose the model, add `"model": "openai/gpt-5-mini"` to the create request.
With no `HARNESS_RUN_TOKEN` set, the server needs no token on `localhost`.
`harness serve -h` lists the flags, and
[`protocol/openapi.json`](protocol/openapi.json) describes every route.

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
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func main() {
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

`Getenv` lets the runtime read the API-key variables of the model providers.
Without it, a provider has no key unless `ModelTransport` supplies the
credentials. `protocol.CreateSession{Model: "openai/gpt-5-mini"}` picks the
model of the session; empty selects the default. `WorkDir` gives each session
the built-in tools, bash among them. Leave it empty for a session with no file
access. A failed turn does not end `Events` with an error, so check
`turn.Wait`.

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

### Give the model your own tools

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

### Run a plugin in the process

The `guard` value of [the plugin step](#block-tool-calls-with-a-plugin) also
runs inside your program, with no separate process and no config entry. Copy
its `guard` variable and `check` function into your program, add the imports
`strings` and `github.com/majorcontext/harness/plugin`, and pass it to the
runtime:

```go
rt, err := harness.New(harness.Options{
	Store:   store.NewMemStore(),
	WorkDir: ".",
	Getenv:  os.Getenv,
	Plugins: []*plugin.Plugin{guard},
})
```

The runtime applies the same hook order, deadlines, and `DenyOnFailure` rule
as for a process plugin. It runs config plugins first and the plugins of
`Options.Plugins` last.

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

[examples/](examples) holds small programs that run. Clone the repository and
run them from its root:

```bash
git clone https://github.com/majorcontext/harness && cd harness
ANTHROPIC_API_KEY=... go run ./examples/prompt "Summarize README.md"
```

- [`prompt`](examples/prompt): send one prompt and stream the reply.
- [`custom-tool`](examples/custom-tool): give the model a Go function to call.
- [`switch-model`](examples/switch-model): continue one conversation on a
  different model or provider.
- [`guardrail`](examples/guardrail): one Go plugin that blocks risky tool
  calls, in process and as a plugin process.

## Configuration

These keys go in either [config file](#config-files). In a model ref
`provider/model`, `provider` is a built-in family (`anthropic`, `openai`) or a
key from `providers`.

### OpenAI-compatible endpoints

Ollama, vLLM, LM Studio, and other chat-completions endpoints take one
`providers` entry. The adapter sends a key on every request, so set one even
if your server does not check it:

```bash
export OLLAMA_API_KEY=ollama
```

Quit the Ollama app first, because it serves port 11434 without the next
setting. Start Ollama with the context window Harness will assume. It runs in
the foreground:

```bash
OLLAMA_CONTEXT_LENGTH=131072 ollama serve
```

In another terminal, pull a model:

```bash
ollama pull llama3.1
```

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

The key becomes the provider name, so the model ref is `ollama/llama3.1`:

```bash
harness run -model ollama/llama3.1 -p "Summarize README.md"
```

Keep `context_window_tokens` equal to the server's window, not the model's
maximum. If the server's window is smaller, it drops context before Harness
compacts. Optional fields: `family` (the wire-quirk tag, defaults to the key)
and `extra_headers` (sent on every request).

### OpenRouter

OpenRouter is an OpenAI-compatible endpoint. Add it as a `providers` entry of
type `openai-compat`:

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

The `extra_headers` field is optional. Then set the key and use the key of the
entry as the provider name:

```bash
export OPENROUTER_API_KEY=...
harness run -model openrouter/anthropic/claude-sonnet-5 -p "Review the diff"
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
`context_window_tokens` as [Context windows](#context-windows) says.
`responses_path` is also valid on the built-in `openai` entry, and nowhere
else.

### Use your ChatGPT subscription (Codex)

A ChatGPT plan can pay for the model calls of the Codex lane. Sign in once:

```bash
harness login codex
```

The command prints a sign-in URL and opens it in your browser. After you sign in, the browser returns to a page on `localhost` and the command exits. On a machine with no browser, run `harness login codex --device`: it prints a code and a link, and you enter the code on another device. `harness login codex --status` prints the account and when the access token ends. It never prints a token. `harness logout codex` deletes the login.

Harness keeps its own login in `auth/codex.json` beside the user config (`~/.harness/auth/codex.json` by default). The file has mode 0600. Harness never reads or writes `~/.codex`, so a login with the Codex CLI and a login with Harness are separate. Harness refreshes the access token before it ends. `harness logout codex` deletes the file; it does not revoke the tokens at OpenAI. The file lock is an exclusive lock on unix only; on Windows it does nothing, so run one Harness process at a time on a login there. If two processes refresh at once, run `harness login codex` again.

Then add an entry that uses the login in place of an API key:

```json
{
  "model": "codex/gpt-5.3-codex",
  "providers": {
    "codex": {
      "type": "openai",
      "auth": "chatgpt",
      "base_url": "https://chatgpt.com/backend-api/codex",
      "responses_path": "/responses",
      "omit_response_params": ["max_output_tokens", "temperature", "top_p", "metadata"],
      "sanitize_tool_schemas": true
    }
  }
}
```

`auth` and `api_key_env` are mutually exclusive, and `auth` is valid only on an entry that builds the OpenAI Responses adapter. The login goes only to the Responses URL of the entry. When the login is missing or the refresh token is rejected, the turn fails with ``not logged in to Codex; run `harness login codex` ``.

To use it in Zed, add Harness as a custom agent server in your Zed settings and put the entry above in `~/.harness/config.json`. Zed shows a "Log in with ChatGPT" button when no login exists: the button opens your browser and stores the login. You can also run `harness login codex` once in a terminal. A client that supports terminal login also shows a terminal method, which runs `harness acp login codex`. If you start Harness through a wrapper script, the script must pass its arguments on (`"$@"`).

```json
{
  "agent_servers": {
    "Harness": { "command": "harness", "args": ["acp"] }
  }
}
```

### Log in with Claude Code

A provider entry of type `claude-code-cli` drives the `claude` CLI, and the CLI keeps its own login. Harness never reads or stores Claude Code credentials. `harness login claude-code` runs `claude auth login` in your terminal, and passes `--console` (API billing) and `--sso` to it. `harness login claude-code --status` runs `claude auth status --text`, and `harness logout claude-code` runs `claude auth logout`. Harness runs the program of `binary_path` of the entry, else `claude` from `PATH`.

In Zed, a provider entry of this type adds a "Log in with Claude Code" button. The button runs `claude auth login` as a child with no terminal, and the CLI opens your browser. A new `authenticate` cancels a sign-in that is still open. When the CLI reports that you are logged out, a new session or a prompt fails with the ACP error "Authentication required", which makes Zed show the button. Logging out from Zed removes only the Harness ChatGPT login. Harness never runs `claude auth logout` from ACP, because the CLI login is shared with every program that uses the CLI; run `harness logout claude-code` for that.

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
`type`, an `openai-compat` or `openai` entry with no `base_url`, an entry with no
`type` under a key other than `anthropic` or `openai`, and a misplaced
`responses_path`.

## Contributing

Read [AGENTS.md](AGENTS.md) first. It holds the repository-wide rules. Read the
`AGENTS.md` in each directory you change. [docs/README.md](docs/README.md) indexes the
technical documentation.

---

Part of [Major Context](https://majorcontext.com).

[Moat](https://github.com/majorcontext/moat) · [Keep](https://github.com/majorcontext/keep) · [Gatekeeper](https://github.com/majorcontext/gatekeeper) · Bailey · **Harness**

MIT licensed. See [LICENSE](LICENSE).
