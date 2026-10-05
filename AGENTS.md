# AGENTS.md

Rules for agents that edit harness. Also read the AGENTS.md in each directory you change.

## Design
- The target architecture is docs/architecture.md. New code follows it. Do not add new concerns to `engine` or `server`.
- Design each interface from its consumer's need. Nothing is kept because it exists. There is no backward compatibility.
- One owner and one source of truth for each piece of state.
- Build session state from the event log with one `Apply` function, live and on replay.
- Give one goroutine each session's state. Other code sends it commands and reads immutable views.
- An interface lives in the package that consumes it. Never branch on a provider or backend name.
- Imports are one-way. Internal packages never import `server` or `cmd`.

## Invariants
- A session is an append-only log of canonical messages, never provider wire objects.
- A repair of live or persisted history is additive-only. Never delete a real tool result.
- An empty tool result never serializes as `null`. Read `ToolResult.SafeContent`.
- Model references use `provider/model`. Aliases resolve before a request reaches a provider.
- Only the engine creates `message.EngineContext`. User text never gains that trust.

## Settled non-goals
Add none of these without a new design decision:
- A permission or approval system for tool calls, or a plan mode.
- A JavaScript runtime, an opencode compatibility layer, or plugin auth hooks.
- A2A support without a concrete cross-organization use case.
- A web UI. `majorcontext/bailey` owns that surface.

## Startup rules
- Keep `harness version` inside the budget of the `Startup budget` step in `.github/workflows/ci.yml`.
- Before first output, read only the user and project config files.
- Make no network call and start no subprocess before a command needs it.
- Add no `init()` side effects. Keep production Go free of cgo.
- Validate credentials on first use. Keep model catalogs static.

## Code
- Keep files at most 800 lines. Keep each function's closing brace at most 80 lines below its opening brace. `internal/gates` compares each changed file with its merge-base version, follows renames, and never blocks a change that only deletes code.
- Write no comment by default. A comment states a constraint, a hazard, or a non-obvious reason.
- A comment never states history: no issue numbers, dates, "previously", or "no longer".
- An exported identifier gets a one-line doc comment.
- Use ASD-STE100 Simplified Technical English for prose. Never print a secret value.
- Write tooling in Go (a test helper or `go run ./internal/...`), not shell scripts.

## Tests
- TDD means: write the failing contract test first, see it fail for the named reason, then implement. A contract test is a scenario row in `e2e/`; a row runs on both drivers with `HARNESS_E2E_RUNTIME=1`. A behavior change adds a row; a bug fix adds one row to the nearest table.
- Write no other test unless the code is pure: `internal/eventlog` Apply, wire transcoders (`provider/*/`, `internal/backend/modelapi/convert_test.go`, `internal/backend/claudecode/frames_test.go`), `config`, `message`, `internal/gates`. Otherwise list the file with a one-line reason in `testdata/test-exceptions.txt` (path, space, reason). `internal/gates` fails a change that adds test lines elsewhere. A listed file still meets the 1.5 test:code ratio of its package.
- Name a test by its behavior, never by an incident. No test reads another package's unexported state.
- No `time.Sleep` or `time.After` in tests. Use `testing/synctest` or channels. Use `internal/testpoll` only for cross-process waits.
- Run Go tests with `-race`.

## Before handoff
`go build ./... && go vet ./... && go test -race ./... && test -z "$(gofmt -l .)"`

## Commits
- Use Conventional Commit subjects.
- The body states the problem, the design, the semantic change, and the verification. Incident detail goes in the body, never in code.
- No AI attribution.
- Add a `CHANGELOG.md` entry under the unreleased heading for each user-visible change.
