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
- **Documentation** — package docs on pkg.go.dev, `engine` examples, and runnable programs in [`examples/`](examples).
