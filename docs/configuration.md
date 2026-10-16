# Configuration

These keys go in either [config file](../README.md#config-files). In a model ref
`provider/model`, `provider` is a built-in provider (`codex` and `claude-code`
use a subscription login; `anthropic` and `openai` use an API key) or a key
from `providers`. The sections for OpenAI-compatible and Responses endpoints
are for API keys.

## Default model

`model` sets the default model. When no config, `HARNESS_MODEL`, or `-model` sets one, `harness run` and `harness acp` choose the first match:

1. The API default, `anthropic/claude-fable-5`, when the key variable of the `anthropic` entry is set (its `api_key_env`, else `ANTHROPIC_API_KEY`).
2. `codex/gpt-6.1-sol`, when the `codex` entry is the native entry or has `auth` `chatgpt`, and the login file exists and is not empty.
3. `claude-code/sonnet`, when the `claude-code` entry has type `claude-code-cli` and its program resolves on `PATH`.
4. The API default.

`harness serve` and the Go API take the API default, unless the embedder sets `Config.SubscriptionDefault`. `harness login codex` and `harness login claude-code` write `model` to the user config when it has none, and print a line that says so. They never change a `model` that is set.

## Web fetch

Set `web` to `{"fetch": true}` to give the model the built-in `web_fetch` tool, which reads public http and https pages. It is off by default. It follows the standard proxy variables: `HTTP_PROXY` for http URLs, `HTTPS_PROXY` for https URLs, and `NO_PROXY` to skip the proxy.

## Web search

Set `web` to `{"search": true}` to let the model use the web search of its provider. It is off by default and works with `anthropic`, `openai`, `codex`, and `claude-code` models; an `openai-compat` model ignores it, and so does a ChatGPT `codex` model that uses the Responses Lite API, because that API refuses the hosted search.

```json
{ "web": { "search": true } }
```

The provider runs the search itself, so a plugin hook cannot block it. A session that lists `allowed_tools` without `web_search` gets no search.

## Use your ChatGPT subscription (Codex)

A ChatGPT plan can pay for the model calls of the Codex lane. Sign in once:

```bash
harness login codex
```

The command prints a sign-in URL and opens it in your browser. After you sign in, the browser returns to a page on `localhost` and the command exits. On a machine with no browser, run `harness login codex --device`: it prints a code and a link, and you enter the code on another device. `harness login codex --status` prints the account and when the access token ends. It never prints a token. `harness logout codex` deletes the login.

Harness keeps its own login in `auth/codex.json` beside the user config (`~/.harness/auth/codex.json` by default). The file has mode 0600. Harness never reads or writes `~/.codex`, so a login with the Codex CLI and a login with Harness are separate. Harness refreshes the access token before it ends. `harness logout codex` deletes the file; it does not revoke the tokens at OpenAI. The file lock is an exclusive lock on unix only; on Windows it does nothing, so run one Harness process at a time on a login there. If two processes refresh at once, run `harness login codex` again.

`codex` is a built-in provider, so no config is needed. After you sign in, use the `codex/` prefix, as in `harness run -model codex/gpt-6.1-sol -p "Reply ok"`. To point it at another URL, write a `codex` entry in `~/.harness/config.json`. The entry replaces the built-in one as a whole, so it must hold every key of the built-in entry:

```json
{
  "providers": {
    "codex": {
      "type": "openai",
      "auth": "chatgpt",
      "base_url": "https://chatgpt.com/backend-api/codex",
      "responses_path": "/responses",
      "omit_response_params": ["max_output_tokens", "temperature", "top_p", "metadata"],
      "sanitize_tool_schemas": true,
      "models_from": "chatgpt"
    }
  }
}
```

`models_from` makes Harness fetch the live model list of the ChatGPT backend; an entry that sets `auth: "chatgpt"` and no `models_from` lists the static models. `auth` and `api_key_env` are mutually exclusive, and `auth` is valid only on an entry that builds the OpenAI Responses adapter. The login goes only to the Responses URL of the entry. When the login is missing or the refresh token is rejected, the turn fails with ``not logged in to Codex; run `harness login codex` ``.

In an editor, add Harness as an [agent server](cli.md#use-harness-in-your-editor). Zed shows a "Log in with ChatGPT" button when no login exists: the button opens your browser and stores the login. You can also run `harness login codex` once in a terminal. A client that supports terminal login also shows a terminal method, which runs `harness acp login codex`. If you start Harness through a wrapper script, the script must pass its arguments on (`"$@"`).

## Log in with Claude Code

`claude-code` is a built-in provider that drives the `claude` CLI, so `harness run -model claude-code/sonnet` needs no config. The CLI keeps its own login. The session runs on Harness tools, so a plugin sees every file and shell call. Harness never reads or stores Claude Code credentials. `harness login claude-code` runs `claude auth login` in your terminal, and passes `--console` (API billing) and `--sso` to it. `harness login claude-code --status` runs `claude auth status --text`, and `harness logout claude-code` runs `claude auth logout`. Harness runs `claude` from `PATH`. To change the program or its arguments, write a `claude-code` entry of type `claude-code-cli` with `binary_path` or `extra_args`: it replaces the built-in entry as a whole.

When you are logged out, a turn fails with ``not logged in to Claude Code; run `harness login claude-code` ``.

In an [ACP client](cli.md#use-harness-in-your-editor) such as Zed, a "Log in with Claude Code" button appears when the program resolves on `PATH`. The button runs `claude auth login` as a child with no terminal, and the CLI opens your browser. A new `authenticate` cancels a sign-in that is still open. When the CLI reports that you are logged out, a new session or a prompt fails with the ACP error "Authentication required", which makes Zed show the button. Logging out from Zed removes only the Harness ChatGPT login. Harness never runs `claude auth logout` from ACP, because the CLI login is shared with every program that uses the CLI; run `harness logout claude-code` for that.

## Let a proxy add the credential

With `auth: "injected"`, Harness sends no credential. A proxy or gateway in front of the endpoint holds the credential and adds it to each request, as gatekeeper does. The entry can be of type `openai`, or the built-in `anthropic` entry. It cannot set `api_key_env`.

```json
{"providers": {"codex": {
  "type": "openai", "auth": "injected",
  "base_url": "https://chatgpt.com/backend-api/codex", "responses_path": "/responses",
  "omit_response_params": ["max_output_tokens", "temperature", "top_p", "metadata"],
  "sanitize_tool_schemas": true, "models_from": "chatgpt",
  "login_url": "https://console.example.com/settings/codex"}}}
```

`models_from: "chatgpt"` makes Harness fetch the model list of the ChatGPT backend through the proxy. For a proxy that replaces placeholders, such as moat, set the placeholders in `extra_headers`: Harness sends them as they are. When the proxy answers 401, the turn fails with `login_required`. A 403 is not a login error. In an editor, the `login_url` is offered as a link, and the editor waits until you have signed in. Only the user config sets `auth`, `login_url`, and `models_from`.

## OpenAI-compatible endpoints

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

To list the models of a gateway or a local server, set `models` on the entry:
`"models": ["llama3.1", "qwen3"]`. `Runtime.Models`, `GET /models`, and the
model picker of an ACP client then list those ids for that provider, in order.
An id in the bundled model tables of Harness gets its name and effort levels
from those tables. An empty or duplicate id fails config loading.

## OpenRouter

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

## OpenAI Responses endpoints

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

## Context windows

Automatic compaction needs the context window of the model. A model that is
missing from the built-in context-window catalog, such as an OpenRouter or
local model, runs with a window of 128000 tokens. Harness logs one warning for
the model, and the session gauge sets `window_estimated`. When the provider
rejects a request as too long, Harness compacts and tries again; it does not
learn the window from the error. To name the window yourself, set
`context_window_tokens`. It applies to every session of a model API provider
and overrides the catalog.

## Config errors

These fail config loading with an error that names the entry: an unknown
`type`, an `openai-compat` or `openai` entry with no `base_url`, an entry with no
`type` under a key other than `anthropic` or `openai`, and a misplaced
`responses_path`.
