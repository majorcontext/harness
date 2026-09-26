# Delegated agent backends: a capability-declared interface

## Motivation

Harness runs one delegated agent CLI today. `engine/claude_code_backend.go`
spawns the `claude` binary, reads its `stream-json` output, and maps each
envelope onto harness messages and events. Nothing about that path is
reusable: `provider/claudecode` exists only to register a family key so
registry lookups succeed, and its own `Client.Stream` is documented as "a
defensive backstop, not a real code path". The real dispatch is
`PromptWithOrigin`/`runAgenticLoop` testing `ClaudeCodeProviderFamily` and
branching before the native provider machinery is reached.

A second delegated agent is now practical. The `codex` CLI ships an
`app-server` subcommand speaking JSON-RPC 2.0 over stdio, with an
`initialize`/`initialized` handshake, `thread/start`, `thread/resume`,
`turn/start`, and a notification stream. Harness already speaks JSON-RPC for
MCP, so the transport costs nothing new.

The question this document answers is not "can we run codex" but "what
interface do two delegated backends share", because the naive answer makes
the result worse than either backend alone.

## 1. Why the intersection is the wrong interface

The obvious move is to define the interface as what both CLIs can do. That
throws away most of what makes the second one worth adding.

Four mechanisms harness hand-rolled for the claude-code lane are first-class
in codex's app-server:

| harness mechanism | codex app-server |
|---|---|
| usage read from the terminal `result` envelope: a whole-turn aggregate, not prompt occupancy | `thread/tokenUsage/updated` notification |
| `/compact` re-implemented per lane | `thread/compact/start` |
| queued prompts packed into an `OPERATOR MESSAGES` template and unpacked by the console | `turn/steer`, which appends input to an active turn |
| `modelmeta`'s static per-ref window tables | `model/list`, reporting models and reasoning efforts |

The first row is the sharpest of the four. Harness does not have a usable
occupancy signal on this lane at all: the `result` envelope reports what a
whole turn spent across every internal API call, which is why a session once
displayed 1,725,110 used tokens against a 1,000,000-token window, and why the
same figure collapses to near zero straight after a compaction. Recovering a
real reading is unmerged work that has taken fourteen review rounds so far.
Codex reports it as one notification.

An intersection interface would reduce all four to the claude-code
implementation and keep the console-side reconciliation those mechanisms
require. The cost is not hypothetical: the queued-prompt machinery alone
(placement staging, operator-batch unpacking, text-equality reconciliation)
took five review rounds and is still the console's most defect-prone code.

## 2. Capability declaration

A backend declares what it natively supports. Harness uses the native path
when a capability is present and falls back when it is absent.

```go
type DelegatedCapabilities struct {
    StreamedTokenUsage bool // usage arrives during a turn, not only at its end
    NativeCompaction   bool // the backend compacts its own history on request
    MidTurnSteer       bool // input can be appended to a turn already running
    ModelDiscovery     bool // the backend enumerates its own models
    ContextWindow      bool // the backend reports the window, not just tokens used
}
```

This is not a new idea in harness so much as an organized one. The same
instinct already exists as scattered booleans: `Config.RequireContextWindow`,
`modelmeta.SuppressUsageGauge`, `contextWindowSourceDisabled`. Each says
"this lane cannot do X". They are unorganized because there has only ever
been one lane to disagree with.

The interface itself stays small. Every delegated backend already does the
same four things:

```go
type DelegatedBackend interface {
    Start(ctx context.Context, cfg DelegatedConfig) (Session, error)
    Capabilities() DelegatedCapabilities
    Turn(ctx context.Context, input TurnInput) (<-chan Event, error)
    Resume(ctx context.Context, id string) (Session, error)
}
```

What varies between backends is not this shape. It is envelope decoding:
which wire object is a message, a tool call, a usage record, a compaction
boundary. That is the bulk of `claude_code_backend.go` and it is where every
delegated-lane defect this quarter originated.

## 3. The context window is absent from both backends

Codex's app-server reports token usage as a `TokenUsageBreakdown`:
`inputTokens`, `cachedInputTokens`, `outputTokens`, `reasoningOutputTokens`,
`totalTokens`. There is no window, limit, or maximum. `contextWindow` appears
in the protocol schema only as an error variant, `contextWindowExceeded`.

Claude Code has the same gap from the other side: the CLI resolves a bare
alias (`--model opus`) to a concrete model harness never learns, so no
static table can supply the denominator either.

Both backends therefore report occupancy without a scale. `ContextWindow`
belongs in the capability set as a capability neither implementation
currently has, and the gauge must keep its honest-unknown state rather than
inventing a plausible denominator. That failure mode is already documented:
a hardcoded 200,000 stand-in rendered a 1,000,000-token session as five
times fuller than it was.

## 4. Codex reports subscription limits natively

The one place codex is strictly richer is quota. `account/rateLimits/read`
plus an `AccountRateLimitsUpdatedNotification` rolling update carry a
`RateLimitSnapshot`:

- `primary` and `secondary` windows, each `{usedPercent, resetsAt, windowDurationMins}`
- `planType`
- `credits`
- `limitName` and `normalModelSlug`, which name a model-specific quota alias
- `rateLimitReachedType`, distinguishing a rate limit from depleted credits

Harness currently reconstructs a thinner version of this by scraping
`x-codex-*` response headers in `provider/openai/subscription_usage.go`. The
app-server path supersedes that scrape and is the only source that reports a
model-specific quota alias, which a console readout needs in order to name
one.

## 5. Sequencing

1. Extract `DelegatedBackend` with Claude Code as the sole implementation and
   an all-false capability set except what it genuinely has. No behavior
   change; the refactor is proven by the existing suite.
2. Add the codex app-server backend behind the interface, declaring its own
   capabilities. Prove the abstraction generalizes before depending on it.
3. Route the mechanisms in section 1 through capabilities, so a backend that
   natively compacts or steers stops paying for harness's workaround.
4. Leave `ContextWindow` false for both until a backend reports a window.

Step 1 is worth doing even if step 2 never happens: the seam it creates is
where every delegated-lane defect this quarter lived, and it is currently
untested in isolation because there is nothing to isolate it from.

## Open questions

- How much of app-server sits behind `experimentalApi: true`. The gate is a
  per-field marker (`ExperimentalField`, registered through `inventory`), not
  a coarse switch, so the answer is per-method and must be enumerated before
  step 2 commits to any of them.
- Whether `turn/steer` preserves prompt identity well enough to retire the
  console's queued-prompt reconciliation, or only reduces it.
- Whether codex threads persisted under `~/.codex/sessions` survive a box
  hibernate and wake, given that directory's placement on the durable mount.
