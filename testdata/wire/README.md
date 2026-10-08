# Recorded wire streams

These files are real model streams, recorded on 2026-10-08 and scrubbed. `TestWireDoublesMatchTheRealWire` in `internal/gates` checks the test doubles (`harnesstest` and `harnesstest/fakeclaude`) and the harness parsers against them.

| Files | Source |
| --- | --- |
| `anthropic.*` | Anthropic Messages API, `claude-haiku-4-5`: text, tool call, tool result turn, extended thinking, and an unknown-model error |
| `chat.*` | OpenAI chat completions, `gpt-4.1-nano`: text, tool call, tool result turn, and an unknown-model error |
| `responses.*` | OpenAI Responses API, `gpt-5-nano`: text with a reasoning summary, tool call, tool result turn, a response cut at the output limit, and an unknown-model error |
| `claudecode.*` | Claude Code CLI 2.1.290, `claude-haiku-4-5`, with the flags of the claudecode backend: a turn with thinking and one Bash call, a turn interrupted during a tool call, and an unknown-model error |

A `.sse` file is the response body of a streaming request. A `.jsonl` file is the stdout of the CLI; strings longer than 300 bytes are cut. An `.error.json` file holds the status and body of a failed request.

Scrubbing (`internal/wirescrub`) replaces ids, request ids, session and account ids, emails, home paths, signatures, encrypted content, and token-like text with stable placeholders. It keeps the structure and every field name. The gate fails when a recording holds text that the scrubber would replace.

`allowed.txt` lists the reviewed exceptions: fields and kinds that a double or a parser has and that no recording holds, each with its reason. The gate fails on an exception that no check needs.

## Record again

The recorder makes real model calls. It runs only when `HARNESS_RECORD_WIRE=1`. It reads `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` from the environment and never writes them. The CLI uses its own login.

```
HARNESS_RECORD_WIRE=1 ANTHROPIC_API_KEY=... OPENAI_API_KEY=... go run ./internal/wirerecord -out testdata/wire
```

Use `-only anthropic,chat,responses,claudecode` to record some wires. Run `go test ./internal/gates` afterward and review any new line that `allowed.txt` needs.
