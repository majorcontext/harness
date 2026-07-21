# Repo-Wide Fuzz & Property-Test Coverage Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (or subagent-driven-development) to implement this plan task-by-task.

**Goal:** Extend the machine-checked testing approach that PR #85 proved out (2 real production bugs found by `FuzzLoadSessionReplay` + the rapid replay model) to every part of the repo where it pays: parsers of externally-controlled bytes and replay/fold state with a mechanical oracle — plus the CI wiring that keeps fuzzing exploring instead of decaying into static table tests.

**Architecture:** Three kinds of artifact. (1) **Native Go fuzz targets** for byte-parsers: seed corpus runs on every ordinary `go test`, exploration happens in a scheduled CI job. (2) **rapid state machines** (test-only dep `pgregory.net/rapid`, already in go.mod) for stateful code with a differential oracle — the oracle is always an independent test-local re-derivation of the documented semantics, so production and oracle can only agree by both matching the spec. (3) **A nightly fuzz workflow** with per-target corpus caching. Production code stays stdlib-only throughout; `go list -deps ./cmd/...` must never contain rapid.

**Where it deliberately does NOT go:** HTTP handler plumbing (typed, table-tested), bash/tool execution (side effects, no oracle), hub UI, anything whose correctness criterion involves model behavior. Concurrency stays with `-race` and deliberate seams, not fuzzing.

**Out of scope for this branch:** OSS-Fuzz integration (the repo is public, so eligible — apply separately after this lands; the targets built here are exactly what OSS-Fuzz consumes).

---

### Task 1: Whole-session replay model — extend the rapid machine beyond queue records

**Files:** `engine/queue_replay_model_test.go` (extend or split into `engine/session_replay_model_test.go`)

The existing machine checks live-vs-replay equality for queue records only. Extend to the rest of the journal so every record type's fold is checked under random interleavings and torn crashes:

- **Goal ops** (provider-free): `RegisterGoal` / `UpdateGoal` / `ClearGoal`. Reference fold gains `goalActive`/`goalCondition` per the documented rules (set → active; updated rewrites condition only while active; achieved/cleared → inactive; stalled/eval_failed/parked are pure trace).
- **Message turns** via a scripted in-test provider (existing engine-test scripted-provider pattern): a `Prompt` op appending a deterministic turn. Reference fold gains message count and cumulative usage (per `record.Usage` replay rules — compact usage adds to cumulative only, never lastUsage).
- **Compaction** if tractable with a scripted summarizer: a `Compact` op; the oracle may reuse `spliceCompact` (same package) for the splice itself — the differential value is WHERE records land under crashes, not re-deriving the splice.
- Torn-crash-reload and crash-reload ops apply across all of it, exactly as today.
- Invariant after every op: live session state (queue, watermark, nextID, goal active/condition, message count, cumulative usage) equals reference fold of the journal file.

Gates: rapid default checks < ~10s; one `-rapid.checks=2000` validation run reported; full engine suite green.

Commit: `test(engine): whole-session replay model — goals, messages, compaction under torn crashes`

### Task 2: Provider stream-transcoder fuzz targets

**Files:** `provider/anthropic/*_fuzz_test.go`, `provider/openai/*_fuzz_test.go`, `provider/openaicompat/*_fuzz_test.go` (names per package convention)

The one place the repo parses truly untrusted wire bytes: provider SSE/event streams. For each provider package, locate the stream-decode entry point (the reader-consuming layer beneath `Stream`/`Next`) and fuzz it with arbitrary bytes:

- Invariants: no panic; no infinite loop (bound input size ~1<<16); any successfully decoded event sequence is protocol-valid (events carry the fields their type requires; a Done event's message, if present, is well-formed per `message` package validation).
- Seed corpus: captured/representative happy-path streams from existing test fixtures, plus truncated and interleaved variants.
- If the decode path is not cleanly reachable without a live HTTP server, fuzz through `httptest` with the fuzz bytes as response body — acceptable but note the per-exec cost; prefer a direct reader-level entry if one exists.

Commit: `test(provider): fuzz SSE/stream transcoders — anthropic, openai, openaicompat`

### Task 3: Skill frontmatter parser fuzz target

**Files:** `skill/*_fuzz_test.go`

The SKILL.md frontmatter parser is hand-rolled and recently needed a block-scalar fix (`58c7d58`) — direct evidence of parser fragility. Fuzz the parse entry point with arbitrary bytes: no panic; accepted inputs yield internally consistent results (e.g. name/description invariants the package documents); include block-scalar seeds (`|`, `>`, chomping variants) from the recent fix's tests.

Commit: `test(skill): fuzz SKILL.md frontmatter parsing`

### Task 4: message + typeid properties

**Files:** `message/*_test.go` (property additions), `typeid/*_fuzz_test.go`

- `message`: rapid properties — Marshal∘Unmarshal round-trip preserves semantic equality on generated messages; `Normalize` is idempotent (`Normalize(Normalize(m)) == Normalize(m)`); `ResolveOrphanToolCalls` output contains no orphan tool calls and is stable on re-application. Generators build arbitrary part combinations (text, tool_call, tool_result, reasoning — whatever the package defines).
- `typeid` + `engine.ValidSessionID`: native fuzz — no panic; parse-accept implies re-encode round-trips.

Commit: `test(message,typeid): round-trip and idempotence properties; id parsing fuzz`

### Task 5: Nightly fuzz CI + stale-comment fix

**Files:** `.github/workflows/fuzz.yml` (new), `.github/workflows/ci.yml` (comment/cache fix)

- New workflow: `schedule` (nightly cron) + `workflow_dispatch`; matrix over every `Fuzz*` target (discover with `grep -r "func Fuzz" --include="*_test.go"` at authoring time and list explicitly); each runs `go test -fuzz <target> -fuzztime 10m` with the corpus dir cached via `actions/cache` keyed per target; on failure, upload the failing corpus entry as an artifact so it can be committed as a regression seed.
- `ci.yml`: the "No go.sum: the module has zero dependencies (by design)" comment is stale since #85 added `go.sum` (test-only rapid). Update the comment to state the actual policy (production stdlib-only; test-only deps allowed) and enable `setup-go` caching now that go.sum exists.

Commit: `ci: nightly fuzz workflow with corpus cache; go.sum-aware setup`

---

## Conventions

- Every task: `go test ./... -count=1` green, `gofmt -l .` empty, `go vet ./...` clean, `go list -deps ./cmd/... | grep -c rapid` → 0, plus a bounded (≥60s) local fuzz session per new target reported in the task's summary. A fuzz-found failure is a production bug: stop, report, fix in a separate preceding commit — never weaken the target.
- Implementers explore entry points themselves; this plan names packages and invariants, not line numbers.
