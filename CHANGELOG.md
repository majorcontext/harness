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
- **`engine.SessionStore`** — a pluggable session journal store. The disk store stays the default.
- **`engine.MemStore`** — an in-memory `SessionStore`. A session on a non-disk store has no index sidecar, no snapshots, and no tool-result retention.
- **`engine.Config.AllowedTools`** — limits a top-level session to the named tools. Nil keeps every tool, an empty slice keeps none, and an unknown name fails the session at its first turn. A delegated claude-code session follows it too: it limits the harness tools that the CLI sees, and a session with a configuration error refuses the turn before the CLI starts.
- **`engine.Config.MaxTurnResumes`** — resumes a root turn that a crash interrupted, up to the given count, instead of closing it as lost to restart. A durable `turn.resumed` record counts each resume. `Config.ResumeRerunTools` re-runs unresolved tool calls.
- **`engine.ClaudeCodeConfig.DisableBuiltinTools` and `Env`** — `DisableBuiltinTools` starts the `claude` child with `--tools ""` and refuses the turn (`ErrClaudeCodeBuiltinTools`) unless the init event lists only `mcp__` tools. `Env` appends `K=V` entries to the child's environment.
- **`engine.ClaudeCodeConfig.MirrorCLISession` and `ConfigRoot`** — mirror the `claude` CLI transcript into `Config.SessionStore` (log `<id>.claude-code`) and restore it into a per-turn scratch `CLAUDE_CONFIG_DIR` before `--resume`, so a delegated session can move between hosts. A failed restore or append fails the turn.
- **`engine.ListSessionIDsFrom`, `SessionExistsIn`, `ReadSessionIndexFrom`, `ReadSessionInfoFrom`, `ReadMessagePageFrom`, and `LoadJournalFrom`** — read the session list, index, message pages, and journal from any `SessionStore`. A `DiskStore` keeps the sidecar and file fast path. Other stores fold the loaded records on each call.
- **`server.Options.Store` and `EventLogID`** — run the HTTP server on any `engine.SessionStore`. The events log becomes the store log `EventLogID` (default `events`), so the SSE `seq` and `Last-Event-ID` replay continue on another process. A server on a non-disk store has no worktree sweep.
- **Documentation** — package docs on pkg.go.dev, `engine` examples, and runnable programs in [`examples/`](examples).
- **Claude Code structured questions** — `harness serve -ask-user-question` lets a Claude Code session ask the user an `AskUserQuestion` question. The turn ends with outcome `awaiting_input` and a `question_call_id`, and `POST /session/{id}/question/{call_id}/answer` resumes it. Off by default.
