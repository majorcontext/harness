# Command line

This page covers the `harness` commands beyond the first run. For the flags of
`run`, run `harness --help`.

## Sessions

`-c` continues the newest session that you started in the current directory, so
the model knows what it did. A session from another directory is never chosen,
and `-c` fails when the current directory has no session. Use `-r` for any
other session.

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

## Give the model more tools

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

A tool call to an MCP server ends after 120 seconds. Set `tool_timeout_s` on the server to change it.

## Work toward a goal

`-goal` runs turns until an independent evaluator model judges the condition
met. Add the evaluator to `.harness.json`. It needs the login or key of its own
provider:

```json
{"goal_evaluator_model": "codex/gpt-6.1-sol"}
```

```bash
harness run -goal "go test ./... passes"
```

The command exits 0 when the goal is met, 3 when the evaluator judges it not met, and 1 when a turn
or the goal evaluator fails.

## Serve it over HTTP

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

To choose the model, add `"model": "codex/gpt-6.1-sol"` to the create request.
With no `HARNESS_RUN_TOKEN` set, the server needs no token on `localhost`.
`harness serve -h` lists the flags, and
[`protocol/openapi.json`](../protocol/openapi.json) describes every route.

A running server can change its MCP servers and plugins with no restart. `PUT /mcp/servers` with `{"mcp_servers": {...}}` and `PUT /plugins` with `{"plugins": [...]}` each replace the whole set. The new set is in memory only: a restart starts from the config again. A running session keeps the tools it started with. A tool that is added later is deferred, and the model loads it with the `mcp` tool.

The server keeps the built-in default model, `anthropic/claude-fable-5`, and does
not look for a subscription login. Set `model` in the config, or in the create
request, to use a subscription.

## Use Harness in your editor

`harness acp` serves the [Agent Client Protocol](https://agentclientprotocol.com)
on standard input and output. In Zed, add it as a custom agent server in your
settings:

```json
{
  "agent_servers": {
    "Harness": { "command": "harness", "args": ["acp"] }
  }
}
```

The client shows a login button for ChatGPT, and for Claude Code when `claude`
is on your `PATH`; see [Use your ChatGPT subscription](configuration.md#use-your-chatgpt-subscription-codex).
The client gets a model and effort picker, image and file attachments, and
session list, load, and resume. When the client supports elicitation forms, a
session can ask you questions in a form.
