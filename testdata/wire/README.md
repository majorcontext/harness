# Recorded wire streams

These files are real model streams, recorded on 2026-10-08 and scrubbed; the `*.websearch.*` files were recorded on 2026-10-14. `TestWireDoublesMatchTheRealWire` in `internal/gates` is the one check of the test doubles (`harnesstest` and `harnesstest/fakeclaude`) and the harness parsers against them. It drives the `fakeclaude` modes that need no harness MCP server, including the park and resume of a question, the second process of the mirror, a queued stdin line, and a SIGINT after the first frame of a mode that hangs. It does not drive the `mcp` and `child_*` modes, and it does not set the environment variants `FAKE_CLAUDE_QUESTION_MIRROR`, `FAKECLAUDE_COMPACT_LOCAL_COMMAND`, `FAKECLAUDE_COMPACT_RESULT`, `FAKE_CLAUDE_DISMISS_DIES`, and `FAKE_CLAUDE_INIT_TOOLS`. The gate checks one direction: what a double emits must be in a recording. A field that a parser reads and a recording holds, but that no double emits, is not checked. No recording holds the stdin frames that the harness writes, or the output of `harnesstest/mcpstdio` and `harnesstest/pluginfixture`.

| Files | Source |
| --- | --- |
| `anthropic.*` | Anthropic Messages API, `claude-haiku-4-5`: text, tool call, tool result turn, extended thinking, a turn with the server tool `web_search_20260209` (`anthropic.websearch.sse`), one whose second search fails (`anthropic.websearch-error.sse`), one that ends with `pause_turn` (`anthropic.websearch-pause.sse`), and an unknown-model error |
| `chat.*` | OpenAI chat completions, `gpt-4.1-nano`: text, tool call, tool result turn, and an unknown-model error |
| `responses.*` | OpenAI Responses API, `gpt-5-nano`: text with a reasoning summary, tool call, tool result turn, a response cut at the output limit, and an unknown-model error. `responses.websearch.sse` is a turn with the built-in `web_search` tool on the ChatGPT Codex backend, `gpt-5.6-sol`, recorded with the login of `harness login codex` |
| `codexmodels.*` | ChatGPT Codex backend `GET /backend-api/codex/models`: the model catalog of a ChatGPT login, with strings cut at 300 bytes (the recorder reads the login of `harness login codex`) |
| `claudecode.*` | Claude Code CLI 2.1.290, `claude-haiku-4-5`, with the flags of the claudecode backend except `--include-partial-messages`, which only the last file uses: a turn with thinking and one Bash call, a turn interrupted during a tool call, an unknown-model error, an AskUserQuestion turn that the defer hook parks and a resume that answers it over the control channel, and `claudecode.partial.jsonl`, a turn with thinking, text, and a Bash call that the CLI streams with `--include-partial-messages`, and `claudecode.websearch.jsonl`, a turn with the built-in `WebSearch` tool, run with `--include-partial-messages`, and `claudecode.builtin-refused.jsonl`, a resumed session whose history holds a Bash call, where the model calls Bash under `--tools ""` and the CLI answers with an error result (recorded with `sonnet`), and `claudecode.unknown-refused.jsonl`, a run on CLI 2.1.300 under `--tools ToolSearch` whose deferred harness `bash` the model calls by its bare name, so the CLI refuses the unknown name, and then loads with `ToolSearch` and calls it |

A `.catalog.json` file is the response body of a request that answers one JSON object. A `.sse` file is the response body of a streaming request. A `.jsonl` file is the stdout of the CLI; strings longer than 300 bytes are cut. An `.error.json` file holds the status and body of a failed request.

Scrubbing (`internal/wirescrub`) replaces ids, request ids, session and account ids, emails, home paths, temporary, socket, and pid paths, signatures, encrypted content and indexes, and token-like text with stable placeholders. It keeps the structure and every field name. The gate fails when a recording holds text that the scrubber would replace.

`allowed.txt` lists the reviewed exceptions: fields and kinds that a double or a parser has and that no recording holds, each with its reason. The gate fails on an exception that no check needs.

## Record again

The recorder makes real model calls. It runs only when `HARNESS_RECORD_WIRE=1`. It reads `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` from the environment and never writes them. The CLI uses its own login.

```
HARNESS_RECORD_WIRE=1 ANTHROPIC_API_KEY=... OPENAI_API_KEY=... go run ./internal/wirerecord -out testdata/wire
```

Use `-only anthropic,chat,responses,claudecode,claudecode-partial,claudecode-builtin-refused,codexmodels,anthropic-websearch,anthropic-websearch-error,anthropic-websearch-pause,codex-websearch,claudecode-websearch,claudecode-toolsearch,claudecode-unknown-refused` to record some wires. The `-websearch` wires need `ANTHROPIC_API_KEY`, the login of `harness login codex`, and the CLI login, in that order. Run `go test ./internal/gates` afterward and review any new line that `allowed.txt` needs.
