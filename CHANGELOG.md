# Changelog

Harness is a fast, extensible, composable agent harness in Go. It ships a headless engine library, a CLI, and an HTTP+SSE server.

Harness is pre-1.0. The engine API, config schema, and plugin protocol may change between minor versions.

## v0.1.0 — Unreleased

First tagged release.

### Added

- **Engine library** (`engine`) — sessions are append-only logs of canonical messages. A session can change provider or model between turns with no history migration. Persisted sessions resume by ID.
- **Providers** (`provider/...`) — Anthropic Messages, OpenAI Responses, any OpenAI-compatible chat-completions endpoint (OpenRouter, Ollama, vLLM), and delegated turns through the Claude Code CLI.
- **CLI** (`cmd/harness`) — `harness run` for one-shot prompts, `-goal` for evaluator-checked goals, `harness serve` for the HTTP+SSE session API, and `harness sessions` to list saved sessions.
- **Tools** — built-in file and shell tools, custom Go tools through `engine.Config.Tools`, subagents through the `task` tool, and managed long-running processes (`process`).
- **Plugins** (`plugin`, `sdk/typescript`) — a language-neutral process protocol with a Go SDK and a TypeScript SDK.
- **MCP** (`mcp`, `mcpserver`) — an MCP client with deferred tool loading, and a Streamable HTTP MCP server.
- **Context management** — automatic and manual compaction, project `AGENTS.md` instructions, and [Agent Skills](https://agentskills.io/specification) (`skill`).
- **Deferred goals** — `POST /session/{id}/goal` accepts `defer: true` to arm a goal without posting its condition as a standalone turn. The loop starts after the next prompt turn and evaluates that turn first; on NOT MET the guidance turn carries the condition. `max_turns` caps the auto-armed loop, for deferred goals and for goals armed behind a busy prompt.
- **Context-window refresh** (`modelmeta`) — `go generate` rewrites the context-window tables from models.dev, and a daily workflow opens a pull request when they change. A new point release is recognized without a hand edit. Session creation still makes no network call.
- **Test model server** (`harnesstest`) — a scripted Anthropic Messages server for tests in other modules. Steps match requests in declaration order, `Block` steps wait for `Release`, `AwaitBlocked` and `AwaitCanceled` observe a blocked request, replies can carry an error message and a `Retry-After` header, and the server fails the test on an unmatched request or a step that never matched.
- **Documentation** — package docs on pkg.go.dev, `engine` examples, and runnable programs in [`examples/`](examples).
- **Claude Code structured questions** — `harness serve -ask-user-question` lets a Claude Code session ask the user an `AskUserQuestion` question. The turn ends with outcome `awaiting_input` and a `question_call_id`, and `POST /session/{id}/question/{call_id}/answer` resumes it. Off by default.
