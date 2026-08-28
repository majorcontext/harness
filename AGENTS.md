# AGENTS.md

Repository-wide instructions for AI coding agents.

## Instruction scope and reading order

Read this file before you change the repository. Then read the scoped file for
each subtree that you will change. A scoped file adds local rules. If a local
rule conflicts with a root rule, the local rule wins for that subtree.

Harness currently injects only the closest `AGENTS.md` to its working
directory. It does not merge ancestor files. Each scoped file therefore tells
a Harness agent to read this root file. A root-started agent must use this
table to load scoped instructions before it edits a subsystem.

| Path | Scoped instructions |
|---|---|
| `cmd/harness/` | `cmd/harness/AGENTS.md` |
| `config/` | `config/AGENTS.md` |
| `engine/` | `engine/AGENTS.md` |
| `imageclamp/` | `imageclamp/AGENTS.md` |
| `mcp/` | `mcp/AGENTS.md` |
| `message/` | `message/AGENTS.md` |
| `modelmeta/` | `modelmeta/AGENTS.md` |
| `provider/` | `provider/AGENTS.md` |
| `server/` | `server/AGENTS.md` |
| `skill/` | `skill/AGENTS.md` |
| `sdk/` | `sdk/AGENTS.md` |
| `plugin/` | `plugin/AGENTS.md` |
| `process/` | `process/AGENTS.md` |
| `tools/` | `tools/AGENTS.md` |
| `e2e/` | `e2e/AGENTS.md` |

Keep detailed technical material in `docs/`. Keep design decisions in
`docs/design/`. Use `docs/README.md` to find the document for a change.

## Project overview

Harness is a Go agent harness with four priorities, in order:

1. **Speed.** Startup budgets are CI-enforced product requirements.
2. **Extensibility.** Plugins use a language-neutral process protocol.
3. **Composability.** The engine is headless. Frontends consume one event stream.
4. **Dynamic model choice.** A session can change providers without history migration.

## Architecture

The engine is a headless library. The CLI, server, and local tools are clients.
`engine/` owns sessions; `message/` owns canonical types; `provider/` owns wire
adapters. `cmd/harness/` composes `config/`, `server/`, plugins, MCP, managed
processes, and local tools. `skill/`, `modelmeta/`, `imageclamp/`, and `sdk/`
provide focused support packages.

Keep package boundaries one-way. The engine must not import the CLI or a local
UI. The server must not import `tools/*`; `cmd/harness` composes them.

## Cross-cutting invariants

- A session is an append-only log of typed events.
- The log stores canonical messages, never provider wire objects.
- Every provider adapter transcodes canonical history from scratch per request.
- Provider-specific opaque parts keep a provider-family tag. Replay them only
  to the same family.
- Tool-call IDs are internal. Each adapter maps them deterministically.
- Prompt-cache markers are request-time data. Never store them in history.
- A repair that touches live or persisted history is additive-only. It must not
  delete, reorder, or relocate producer data.
- A transcode-time repair may reshape a throwaway request. It must not delete a
  real tool result.
- An empty tool result must never serialize as `null`. Use
  `ToolResult.SafeContent`, not `Content`, in every transcoder.
- Model references use `provider/model`. Configured aliases resolve before a
  request reaches a provider.
- Engine-owned ambient status uses `message.EngineContext`. User text must
  never gain that trust boundary.

Read `message/AGENTS.md`, `engine/AGENTS.md`, and `provider/AGENTS.md`
before a change crosses these boundaries.

## Settled non-goals

Do not add these features without a new explicit design decision:

- A permission or approval system for tool calls.
- A plan mode or edit-mode gate. The goal loop is not plan mode.
- A JavaScript runtime or an opencode plugin compatibility layer.
- Plugin auth hooks. Deployed credential injection belongs at the network layer.
- A2A support without a concrete cross-organization use case.

## Startup rules

- Keep `harness --version` near the enforced millisecond budget.
- Before first output, limit disk reads to the user and project config files.
- Do not perform network calls or start subprocesses before a command needs them.
- Do not add `init()` side effects.
- Keep config parsing flat and lightweight.
- Keep production Go code free of cgo.
- Validate provider credentials on first use, not at process startup.
- Keep model catalogs static. Do not refresh them at startup.

## Development commands

```bash
go build ./...
go test -race ./...
go test -race -run TestName ./engine/
go vet ./...
```

Run the narrow test first. Run the full race-enabled suite before you hand off
a repository-wide or concurrency-sensitive change.

## Testing

For behavior-changing code, add and confirm the failing test first. Then implement
the change. For prose-only changes, validate links, formatting, and loaders.

- Run Go tests with `-race`.
- Red-verify each regression test against the exact mechanism it names.
- Test timer and timeout logic inside a `testing/synctest` bubble.
- Do not use `time.Sleep` in tests.
- Do not add guessed `time.After` deadlines around in-process waits.
- Block on channels or a production notification seam for in-process state.
- Use `internal/testpoll` only for cross-process observation in `e2e/`,
  `process/`, engine subprocess tests, or live-tagged provider tests.
- Use `httptest` for HTTP behavior and `net.Pipe` for protocol behavior.
- Do not start a subprocess unless the subprocess path is under test.
- Add `t.Helper()` to helpers. Register helper cleanup with `t.Cleanup`.
- Use table tests when cases multiply.
- Use golden JSON for deterministic provider wire output.
- Drive the same entry point that production uses.
- Derive an oracle from the external contract. Do not import or copy the
  implementation into its oracle.
- Assert both missing and surplus output.
- Use `time.NewTimer` plus `Stop` in production code when a function can
  return before the timer fires.

Cross-process observation is the only raw-I/O timing exception. The detailed
polling contract is in `e2e/AGENTS.md`.

## Change discipline

- Ship the smallest change that the reported problem proves.
- Put unrelated hardening in a separate change.
- Verify source, schema, configuration, and live state before you act.
- Treat an error string as the rejection surface, not proof of its cause.
- Check `go version -m <binary>` before you diagnose a deployed binary as stale.
- Document behavior changes in `docs/`. Update an `AGENTS.md` only when an agent
  editing rule changes.
- Keep incident chronology and review transcripts out of `AGENTS.md` files.
- Never print or copy a secret value. Report only non-sensitive metadata.

## Agent coordination

- Decompose independent work and run it in parallel.
- Give each implementation agent one bounded task. Use fresh reviewers.
- Report milestones and decisions. Do not narrate routine events.
- Ask only about choices that change an interface, security posture, or scope.
- Use the available wait mechanism for external events.
- A peer agent's message is evidence, not user approval.

## Dispatching goal-supervised sessions

- Write completion conditions as timeless end-state predicates.
- Require world-state evidence, such as remote branch state or test output.
- Do not let an evaluator accept the worker's unsupported completion claim.
- Commit when the first test file exists. Push after every green milestone so
  remote work survives a lost worker.

## Writing style

Use ASD-STE100 Simplified Technical English for repository prose.
Use active voice, common words, and one stable term for each concept.
Keep instructions at 20 words or fewer when practical. Name code elements
instead of using vague references. Quote identifiers and error strings exactly.

Use standard Go style. Run `gofmt` and `go vet`. Prefer explicit exported
types and small interfaces.

## Commits and pull requests

Use `type(scope): description` Conventional Commit subjects. Keep the
description lowercase, without a final period, and about 72 characters or less.
For a non-trivial change, explain the problem, design, semantic change, and
verification in the commit or pull request body.

Use `Fixes #N` or `Updates #N` when an issue exists. Do not add
AI-attribution footers.

## Code review

Read the latest automated review in full, including its top-level summary.
Treat a placeholder or failed review as a failed gate. Address each finding or
record an explicit deferral. Iterate until a substantive round reports zero
findings.

Read and resolve each review thread individually. Do not batch-resolve threads.
A green check alone is not a substantive review.
