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
- **Test model server** (`harnesstest`) — a scripted model server for tests in other modules. `New` speaks the Anthropic Messages wire; `NewChat` speaks the OpenAI chat-completions wire, with `Reply.Reasoning` and `Reply.ErrorCode`, and fails the test on a request that a real gateway rejects. Steps match requests in declaration order, `Block` steps wait for `Release`, `AwaitCanceled` observes a client that dropped a blocked request, replies can carry an error message and a `Retry-After` header, and the server fails the test on an unmatched request or a step that never matched.
- **Event-sink test receiver** (`harnesstest`) — `NewSinkReceiver` starts a scripted event-sink receiver that records every batch as a `SinkBatch`, answers each one through a `SinkReply`, and lets a test wait on the received batches with `Await`.
- **Documentation** — package docs on pkg.go.dev, `engine` examples, and runnable programs in [`examples/`](examples).
- **Claude Code structured questions** — `harness serve -ask-user-question` lets a Claude Code session ask the user an `AskUserQuestion` question. The turn ends with outcome `awaiting_input` and a `question_call_id`, and `POST /session/{id}/question/{call_id}/answer` resumes it. Off by default.

### Fixed

- **Goal exhaustion** — a goal loop that spends `max_turns` without a MET verdict now ends the goal. The goal journals `goal.cleared` with `goal_reason` `goal exhausted max_turns (N)` before `turn.end` with outcome `max_turns_exceeded`, and the session reads `idle`. The goal used to stay active, so the session read `goal-running` forever.
