# AGENTS.md

Rules for agents that edit harness. Also read the AGENTS.md in each directory you change.

## Design
- The target architecture is docs/architecture.md. New code follows it.
- Design each interface from its consumer's need. Nothing is kept because it exists. There is no backward compatibility.
- One owner and one source of truth for each piece of state.
- Build session state from the event log with one `Apply` function, live and on replay.
- Give one goroutine each session's state. Other code sends it commands and reads immutable views.
- An interface lives in the package that consumes it. Never branch on a provider or backend name.
- Imports are one-way. Internal packages never import `cmd`.

## Invariants
- A session is an append-only log of canonical messages, never provider wire objects.
- A repair of live or persisted history is additive-only. Never delete a real tool result.
- An empty tool result never serializes as `null`. Read `ToolResult.SafeContent`.
- Model references use `provider/model`. Aliases resolve before a request reaches a provider.
- Only the runtime creates `message.EngineContext`. User text never gains that trust.

## Spec and review
- `docs/architecture.md` is the binding spec. Cite the sections and lines a change implements in the PR body. When the spec is silent or unclear, stop and ask; do not invent.
- A difference from the spec is a blocker. A spec change needs a dated Andy decision recorded in the spec text (`Decided`, `Deliberate parity breaks`), in the same PR or a docs PR.
- Keep parity with the old engine. Add no feature the spec does not list; simplify structure, not behavior.
- Judge a change against the architecture as a whole, not the diff: answer `.github/skills/architecture-review/SKILL.md` for every review.
- One concern per PR. Describe the change in the PR body: problem, design, semantic change, verification. Do not hard-wrap a PR body.
- Never resolve a review thread without a reply: fix it and say how, or say why you decline.

## Gates (`internal/gates`, `docs/architecture.md` `CI gates`)
- Routes: mounted routes equal the spec's route table and `protocol/openapi.json`. Errors: each code is a `protocol` constant, a sentinel, and a row of the `Errors` table with one status; each producer owns its codes, and one writer builds the error body.
- Structure: `internal/gates/structure.go` lists the one owner of each concept (context window, `Store.Append`, `State.Apply`, `message.EngineContext`) and bans provider-name branches outside `config`, the router, and provider wires. An allow-list entry carries a date and a reason; a stale entry fails.
- Wire: a test double emits only what `testdata/wire` holds; a recording holds no secret or id. Refresh a recording with `HARNESS_RECORD_WIRE=1`. Every `e2e/` HTTP client comes from `wireClient`.

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
- Use ASD-STE100 Simplified Technical English for prose. Never print or log a secret value.
- Write tooling in Go (a test helper or `go run ./internal/...`), not shell scripts.
- After a change to a `protocol` type or to `internal/server` `Table`, commit the `go generate ./protocol` output (`protocol/openapi.json`, `protocol/protocol.ts`). CI fails on a diff.

## Tests
- TDD means: write the failing contract test first, see it fail for the named reason, then implement. A contract test is a scenario row in `e2e/`; a row runs on both drivers with `HARNESS_E2E_RUNTIME=1`, except a row marked `rowDeleted` (the spec deletes what it pins). A behavior change adds a row; a bug fix adds one row to the nearest table.
- Write no other test unless the code is pure: `internal/eventlog` Apply, wire transcoders (`internal/provider/*/`, `internal/backend/modelapi/convert_test.go`, `internal/backend/claudecode/frames_test.go`), `config`, `internal/message`, `internal/gates`. Otherwise list the file with a one-line reason in `testdata/test-exceptions.txt` (path, space, reason). `internal/gates` fails a change that nets new lines inside Test, Benchmark, Fuzz, or Example functions or package-level vars elsewhere. Top-level helpers, fakes, and types do not count. A listed file still meets the 1.5 test:code ratio of its package.
- Name a test by its behavior, never by an incident. No test reads another package's unexported state.
- No `time.Sleep` or `time.After` in tests. Use `testing/synctest` or channels. Use `internal/testpoll` only for cross-process waits.
- Run Go tests with `-race`. `gofmt` output differs across Go minor versions; CI reads the version from `go.mod`.

## Before handoff
`go build ./... && go vet ./... && go test -race ./... && test -z "$(gofmt -l .)"`

## Commits
- Use Conventional Commit subjects.
- The body states the problem, the design, the semantic change, and the verification. Incident detail goes in the body, never in code.
- No AI attribution.
- Add a `CHANGELOG.md` entry under the unreleased heading for each user-visible change.
