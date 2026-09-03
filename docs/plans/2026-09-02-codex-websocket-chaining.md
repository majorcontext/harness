# Codex WebSocket Response Chaining Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Codex-only Responses WebSocket `previous_response_id` chaining and bounded `generate:false` startup prewarm.

**Architecture:** The engine continues to build complete canonical requests. The Codex WebSocket pool keeps runtime-only lineage, validates a new full request against its previous request and completed output, then sends only the suffix. A fresh session starts a bounded background prewarm through an optional provider capability; the first real turn consumes it or safely uses the complete request.

**Tech Stack:** Go, `github.com/coder/websocket`, canonical Harness provider interfaces, JSONL/session metrics, race-enabled Go tests.

**Spec:** `docs/design/codex-websocket-chaining.md`

## Global Constraints

- Apply chaining and prewarm only to `openai.CodexFamily` with WebSocket transport enabled.
- Keep `store:false` and `include:["reasoning.encrypted_content"]`.
- Keep canonical history and persisted session formats unchanged.
- Keep a complete request body available for every HTTP fallback.
- Treat uncertain lineage as a full request, never as authorization to send a suffix.
- Never log or emit a response ID value.
- Bound the complete startup-prewarm task to 15 seconds.
- Use TDD and run every changed Go package with `-race`.

---

### Task 1: Codex incremental request projection

**Files:**
- Modify: `provider/openai/transcode.go`
- Modify: `provider/openai/ws.go`
- Create: `provider/openai/ws_chaining_test.go`

**Interfaces:**
- Produces an exhaustive `responsesRequestPropertiesMatch(previous, current *apiRequest) bool` helper.
- Produces a pure `incrementalInput(previous *apiRequest, responseItems, current []json.RawMessage) ([]json.RawMessage, bool)` helper.
- Extends WebSocket framing with optional `previous_response_id`, `generate`, and input override values without changing the complete HTTP body.

- [ ] **Step 1: Write failing projection and wire-shape tests**

Add table tests that prove:

```go
func TestIncrementalInputUsesSuffixAfterRequestAndResponsePrefix(t *testing.T)
func TestIncrementalInputRejectsChangedOrShortPrefix(t *testing.T)
func TestResponsesRequestPropertiesMatchCoversEveryField(t *testing.T)
func TestResponseCreateAddsPreviousResponseIDAndSuffix(t *testing.T)
func TestResponseCreatePrewarmAddsGenerateFalse(t *testing.T)
```

The property test must change each context-bearing `apiRequest` field one at a
time and require a mismatch. The frame tests must assert the exact decoded JSON,
including absence of `stream` and absence of chaining fields on a normal full
request.

- [ ] **Step 2: Verify RED**

Run:

```bash
go test -race ./provider/openai -run 'Test(IncrementalInput|ResponsesRequestPropertiesMatch|ResponseCreate)'
```

Expected: build failure for missing helpers or assertion failure because the
current frame always contains the complete input and has no chaining fields.

- [ ] **Step 3: Implement pure projection and framing**

Add a small WebSocket request options type:

```go
type responseCreateOptions struct {
    PreviousResponseID string
    Input              []json.RawMessage
    InputSet           bool
    Generate           *bool
}
```

Decode the complete body into `apiRequest`, apply only the WebSocket projection,
and marshal `response.create`. Compare ordered JSON values semantically while
preserving deterministic output bytes. Keep the property match exhaustive so a
new `apiRequest` field requires a deliberate comparison decision.

- [ ] **Step 4: Verify GREEN**

Run the RED command and then:

```bash
go test -race ./provider/openai
```

- [ ] **Step 5: Commit and push**

```bash
git add provider/openai/transcode.go provider/openai/ws.go provider/openai/ws_chaining_test.go
git commit -m "feat(provider): project incremental codex requests"
git push
```

---

### Task 2: Runtime lineage and clean-completion updates

**Files:**
- Modify: `provider/openai/openai.go`
- Modify: `provider/openai/ws_pool.go`
- Modify: `provider/openai/ws_stream.go`
- Modify: `provider/openai/ws_test.go`
- Modify: `provider/openai/ws_chaining_test.go`

**Interfaces:**
- Consumes Task 1 projection helpers.
- Produces `wsLineage`, owned by one `wsPoolEntry`, containing a complete request, response ID, response output items, and connection generation.
- Produces one completion callback from `stream.handle` to the pool.

- [ ] **Step 1: Write failing end-to-end lineage tests**

Add tests for:

```go
func TestWebSocketSecondTurnSendsOnlyIncrementalSuffix(t *testing.T)
func TestWebSocketToolRoundContinuesResponseLineage(t *testing.T)
func TestWebSocketFullMismatchReestablishesLineage(t *testing.T)
func TestWebSocketIncompleteAndFailedResponsesClearLineage(t *testing.T)
func TestWebSocketStaleGenerationCannotRearmLineage(t *testing.T)
func TestWebSocketConcurrentFallbackCannotRearmLineage(t *testing.T)
func TestWebSocketResponseItemsMatchTextCallsAndReasoning(t *testing.T)
```

Drive `Client.Stream` through an `httptest` WebSocket server. Inspect every
`response.create` frame. For the second compatible request, require
`previous_response_id` and only the suffix. For mismatch, require the complete
input and no ID.

- [ ] **Step 2: Verify RED**

Run:

```bash
go test -race ./provider/openai -run 'TestWebSocket(SecondTurn|ToolRound|FullMismatch|Incomplete|StaleGeneration|ConcurrentFallback|ResponseItems)'
```

Expected: assertions show the second frame still contains complete history and
no `previous_response_id`.

- [ ] **Step 3: Implement lineage ownership**

Add pool-entry lineage and increment a generation whenever a connection is
invalidated or replaced. Record the complete logical request before send, but
publish lineage only from a clean `response.completed` callback for the same
generation.

For normal inference, derive response output items from the completed canonical
assistant message with the existing OpenAI message transcoder. Use an explicit
empty output list for prewarm. Do not update lineage for incomplete, failed,
canceled, or truncated responses.

A property/prefix mismatch sends a full frame on the healthy socket. Its clean
completion replaces lineage.

- [ ] **Step 4: Verify GREEN and regression behavior**

Run the RED command and:

```bash
go test -race ./provider/openai
go test -race ./provider/...
```

- [ ] **Step 5: Commit and push**

```bash
git add provider/openai
git commit -m "feat(provider): retain codex websocket lineage"
git push
```

---

### Task 3: Chain-miss recovery and request-mode observability

**Files:**
- Modify: `provider/provider.go`
- Modify: `provider/openai/openai.go`
- Modify: `provider/openai/ws.go`
- Modify: `provider/openai/ws_pool.go`
- Modify: `provider/openai/ws_stream.go`
- Modify: `provider/openai/ws_chaining_test.go`
- Modify: `engine/engine.go`
- Modify: `engine/turn_metrics_test.go`

**Interfaces:**
- Adds non-secret request transport metadata to terminal provider events: mode, complete item count, sent item count, and previous-response use.
- Adds the corresponding optional fields to `engine.TurnMetrics`.
- Produces one same-socket full retry for an immediate `previous_response_not_found` chain miss.

- [ ] **Step 1: Write failing recovery and metrics tests**

Add tests that prove:

```go
func TestPreviousResponseNotFoundRetriesFullRequestOnce(t *testing.T)
func TestPreviousResponseNotFoundAfterVisibleOutputDoesNotRetry(t *testing.T)
func TestPreviousResponseNotFoundSecondFailureEscapes(t *testing.T)
func TestTurnMetricsReportsCodexIncrementalProjection(t *testing.T)
```

The first server sequence must return an immediate chain-miss error, then accept
a complete frame on the same socket. Assert the response ID value is absent from
all metric values and serialized records.

- [ ] **Step 2: Verify RED**

Run:

```bash
go test -race ./provider/openai -run 'TestPreviousResponseNotFound'
go test -race ./engine -run TestTurnMetricsReportsCodexIncrementalProjection
```

Expected: chain miss escapes as an error and terminal metadata has no projection
fields.

- [ ] **Step 3: Implement bounded recovery and metadata**

Parse `previous_response_not_found` as a typed internal chain miss. Before any
model-visible output, clear lineage, keep the socket, and resend the immutable
complete request once. After visible output, invalidate the socket and return a
truncated-stream error. Never retry a second chain miss locally.

Carry only enums, booleans, and counts into terminal events and turn metrics.
Never carry the response ID.

- [ ] **Step 4: Verify GREEN**

Run both RED commands and:

```bash
go test -race ./provider/openai ./engine
```

- [ ] **Step 5: Commit and push**

```bash
git add provider engine
git commit -m "feat(engine): report codex request projection"
git push
```

---

### Task 4: Codex `generate:false` provider prewarm

**Files:**
- Modify: `provider/provider.go`
- Modify: `provider/openai/openai.go`
- Modify: `provider/openai/transcode.go`
- Modify: `provider/openai/ws_pool.go`
- Modify: `provider/openai/ws_test.go`
- Create: `provider/openai/prewarm_test.go`

**Interfaces:**
- Adds optional capability:

```go
type StartupPrewarmer interface {
    Prewarm(context.Context, *Request) error
}
```

- `openai.Client.Prewarm` is effective only for Codex family plus WebSocket transport.
- Allows empty transcodable input only in the internal prewarm path.

- [ ] **Step 1: Write failing provider prewarm tests**

Add:

```go
func TestCodexPrewarmSendsGenerateFalseAndEmptyInput(t *testing.T)
func TestCodexPrewarmEstablishesEmptyOutputLineage(t *testing.T)
func TestOpenAIFamilyPrewarmDoesNothing(t *testing.T)
func TestOrdinaryRequestStillRejectsEmptyInput(t *testing.T)
func TestPrewarmFailureLeavesFullRequestAvailable(t *testing.T)
```

Assert exact frame shape, `store:false`, no user input, no assistant event, and a
first real request with the warmup response ID plus its suffix.

- [ ] **Step 2: Verify RED**

Run:

```bash
go test -race ./provider/openai -run 'Test(CodexPrewarm|OpenAIFamilyPrewarm|OrdinaryRequestStill|PrewarmFailure)'
```

Expected: `StartupPrewarmer` and `Prewarm` are absent.

- [ ] **Step 3: Implement provider prewarm**

Use the same transcode, URL, authorization, proxy, schema sanitization, parameter
omission, pool entry, and protocol as `Stream`. Add an internal transcode option
for empty prewarm input. Send `generate:false`, consume through
`response.completed`, and publish lineage with no response output items.

Do not emit a provider assistant event or token usage to the engine.

- [ ] **Step 4: Verify GREEN**

Run the RED command and:

```bash
go test -race ./provider/...
```

- [ ] **Step 5: Commit and push**

```bash
git add provider
git commit -m "feat(provider): prewarm codex websocket sessions"
git push
```

---

### Task 5: Bounded asynchronous engine startup prewarm

**Files:**
- Modify: `engine/engine.go`
- Modify: `engine/session_manager.go`
- Modify: `engine/instructions.go`
- Modify: `engine/skills.go`
- Create: `engine/startup_prewarm.go`
- Create: `engine/startup_prewarm_test.go`

**Interfaces:**
- Produces a session-owned startup-prewarm handle with start time, cancel function, completion channel, and one-consumer resolution.
- Produces a shared request-assembly helper used by prewarm and normal turns.
- Keeps normal turn counters, notification checkout, ambient context, compaction, and history mutation outside prewarm assembly.

- [ ] **Step 1: Write failing lifecycle tests**

Add tests inside deterministic synchronization bubbles for:

```go
func TestNewSessionReturnsWhileStartupPrewarmBlocked(t *testing.T)
func TestFirstPromptConsumesReadyStartupPrewarm(t *testing.T)
func TestFirstPromptWaitsOnlyForRemainingPrewarmDeadline(t *testing.T)
func TestPromptCancellationCancelsStartupPrewarm(t *testing.T)
func TestStartupPrewarmFailureDoesNotFailPrompt(t *testing.T)
func TestStartupPrewarmCachesInstructionAndSkillErrors(t *testing.T)
func TestStartupPrewarmPropertyDriftFallsBackFull(t *testing.T)
func TestStartupPrewarmEmitsNoTurnMessageOrUsage(t *testing.T)
func TestChildPrewarmStartsAfterToolRestriction(t *testing.T)
```

Use channels, injected clocks/timers, and fake providers. Do not use
`time.Sleep` or guessed `time.After` waits.

- [ ] **Step 2: Verify RED**

Run:

```bash
go test -race ./engine -run 'Test(NewSessionReturnsWhileStartup|FirstPrompt|PromptCancellationCancelsStartup|StartupPrewarm|ChildPrewarm)'
```

Expected: startup prewarm types and calls are absent.

- [ ] **Step 3: Implement construction and assembly lifecycle**

Factor stable request assembly from `streamTurn` without changing ordinary
request order. Start prewarm only after each root or child session has its final
ID, model, provider, and tool restrictions. Use a dedicated 15-second context
for the complete task. `NewSession` and manager constructors must return without
waiting.

The first real native turn consumes the handle once. It waits only until the
original deadline, cancels on prompt cancellation, and otherwise proceeds with
normal full request behavior. Instruction and Skill discovery cache their result
for the first prompt. Prewarm-specific provider, hook, MCP, or transport errors
remain best-effort and do not become session errors.

Do not increment the turn, call `OnRequest`, check out notifications, inject
ambient context, mutate history, compact, or accumulate usage for prewarm.

- [ ] **Step 4: Verify GREEN and race safety**

Run the RED command and:

```bash
go test -race ./engine
go test -race ./server ./cmd/harness
```

- [ ] **Step 5: Commit and push**

```bash
git add engine
git commit -m "feat(engine): schedule codex startup prewarm"
git push
```

---

### Task 6: Documentation, full verification, and review

**Files:**
- Modify: `docs/models-and-providers.md`
- Modify: `docs/engine-request-cycle.md`
- Modify: `provider/AGENTS.md`
- Modify: `engine/AGENTS.md`
- Modify if implementation differs: `docs/design/codex-websocket-chaining.md`

**Interfaces:**
- Documents the shipped behavior and editing invariants.

- [ ] **Step 1: Update current-behavior documentation**

Document Codex-only gating, runtime lineage, full fallback, startup disclosure,
15-second whole-task deadline, chain-miss recovery, and metrics. Keep design and
runtime documents consistent with final code.

- [ ] **Step 2: Run focused verification**

```bash
go test -race ./provider/openai ./provider/... ./engine ./server ./cmd/harness
go vet ./provider/... ./engine/... ./server/... ./cmd/harness/...
test -z "$(gofmt -l provider engine server cmd/harness)"
git diff --check
```

- [ ] **Step 3: Run repository-wide verification**

```bash
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
git diff --check
```

- [ ] **Step 4: Commit and push**

```bash
git add docs provider/AGENTS.md engine/AGENTS.md
git commit -m "docs(provider): document codex response chaining"
git push
```

- [ ] **Step 5: Request substantive review**

Review the complete diff from the design commit's parent through branch HEAD.
Fix every critical or important finding. Re-run affected focused tests after
each fix and the full verification after the final fix.

- [ ] **Step 6: Create or update the pull request**

Use a Conventional Commit-style PR title. Explain the observable problem, why
runtime-only lineage solves it, exact semantic changes, privacy boundary, and
fresh verification evidence.
