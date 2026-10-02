# AGENTS.md

Rules for agents that edit harness. Also read the AGENTS.md in each directory you change.

## Design
- The target architecture is docs/architecture.md. New code follows it. Do not add new concerns to `engine` or `server`.
- Design each interface from its consumer's need. Nothing is kept because it exists. There is no backward compatibility.
- One owner and one source of truth for each piece of state.
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
- Keep `harness --version` near its enforced millisecond budget.
- Before first output, read only the user and project config files.
- Make no network call and start no subprocess before a command needs it.
- Add no `init()` side effects. Keep production Go free of cgo.
- Validate credentials on first use. Keep model catalogs static.

## Code
- Keep files under 600 lines and functions under 80. `internal/gates` enforces the ceilings.
- Write no comment by default. A comment states a constraint, a hazard, or a non-obvious reason.
- A comment never states history: no issue numbers, dates, "previously", "no longer", or "instead of".
- An exported identifier gets a one-line doc comment.
- Use ASD-STE100 Simplified Technical English for prose. Never print a secret value.

## Tests
- The contract suite in `e2e/` pins behavior: scenario tables, `internal/fakemodel`, golden observations.
- A behavior change adds or changes a scenario row. A bug fix adds one row to the nearest table.
- Name a test by its behavior, never by an incident.
- Unit tests cover pure functions. No test reads another package's unexported state.
- No `time.Sleep` or `time.After` in tests. Use `testing/synctest` or channels. Use `internal/testpoll` only for cross-process waits.
- Run Go tests with `-race`.

## Before handoff
`go build ./... && go vet ./... && go test -race ./... && test -z "$(gofmt -l .)"`

## Commits
- Use Conventional Commit subjects.
- The body states the problem, the design, the semantic change, and the verification. Incident detail goes in the body, never in code.
- No AI attribution.
- Add a `CHANGELOG.md` entry under the unreleased heading for each user-visible change.
