# Fleet and serve diagnostics

This document describes fleet state, task lineage, provider exhaustion, and
serve diagnostics.

## Fleet model (the deploy story)

The full build spec lives in `docs/design/fleet-model.md` — read it before
touching anything box-identity, session-lineage, or goal-pause related. The
short version this repo's code assumes: identity is an operator-chosen box
**NAME**; storage is one volume/directory per name (`HARNESS_SESSION_DIR`
points at it), never shared between concurrently-live servers; a box is
ephemeral compute serving one name (cattle), the name and its volume are
durable (pets). Respawning the same name over the same volume is **ADOPT**
— history restores, and any goal that was armed when the box died surfaces
as `paused`/`pause_reason: "restart"` (see the goal loop's paused
presentation, `engine/goal.go` and `server/journal.go`'s `goal.paused`
record) rather than a false "still running" reading. `parent_session`
(`POST /session`, see `engine/store.go`) is the lineage thread connecting a
re-dispatch to the task it continues from, so a fleet UI can group a box's
history by task across boxes.

Subagent lineage is durable. `SessionManager.Spawn` records
`task_parent_id`, `task_agent_type`, and `task_depth` on the child's
session header (`engine/store.go`), and appends each child id to the
parent's own log. `LoadSession` restores all of them with no
SessionManager adoption needed. `GET /session/{id}.lineage` prefers the
durable `task_depth` over the live tree's derived depth, and merges live
children with the durable spawn list (`childIDsUnion`,
`server/handlers.go`) — so `lineage.depth` and `lineage.children` survive
`Reap` and a process restart. `childIDsUnion` merges both sides through
ONE de-duplicating loop and trusts neither side to be duplicate-free: an
id appears exactly once, whichever side carried it. Never re-add a
per-side fast path that skips the merge — an earlier one copied `live`
verbatim when `durable` was empty, so one repeated id survived or
collapsed depending only on whether the OTHER argument had anything in
it. A legacy header without `task_depth`
restores 0; `adoptReloadedLocked` then falls back to the `m.maxDepth`
refusal sentinel, exactly as before the field existed.

A failed child's `fail_reason` carries the CAUSE, not only a class.
`classifySpawnFailure` (`engine/session_manager.go`) builds it as a fixed
classified prefix, then the underlying error message — masked with
`maskSecrets` and capped at `spawnErrorDetailCap` (500) runes. One prefix
covers a whole family of causes (a permanent 400 is a malformed request
AND a quota rejection AND a policy refusal), so a parent that reads only
the prefix must guess: a live incident measured that guess as "respawn a
sibling straight into the same fleet-wide provider wall". The #82 leak
rule still holds in its narrower form — never surface a provider error
RAW — through masking plus the cap, the same best-effort trade a retained
tool result already makes. `context.Canceled`/`context.DeadlineExceeded`
keep their short fixed `canceled`/`timed out` strings, with no cause
appended. The reason reaches the parent through the `[tasks: ...]`
notification, `SessionNode.FailReason` (so `task status` and
`GET /session/{id}.lineage.fail_reason`), and the journal's
`task_fail_reason`.

Server-side session resolution has ONE entry point: `Server.resolveLive`
returns a `liveSession` snapshot (`server/live.go`) that holds the
residency half (`Server.sessions`, one `s.mu` hold) and the SessionManager
half (one `SessionAndInfo` hold) together. Read a session, its status, or
its lineage from that snapshot — never from `s.sessions` or `sessMgr`
directly, and never take a second manager read later in the same request.
The two halves are separate holds on purpose: `server.mu` is a leaf lock
with respect to `SessionManager.mu`, so one atomic hold over both would
build the cycle that rule forbids. Residency wins whenever it has an
answer, because a resident session's own `running` flag is authoritative
for itself (`freeRunSlotAndEmitIdle` clears it before `ReportTurnEnd`
flips the node). The manager half answers only what residency cannot: a
Spawn-driven child, which is never a residency key.

**Provider exhaustion is not a child failure.** An ACCOUNT-level supply
wall — the API key's usage limit, quota, credit balance, or spend cap — is
FLEET-WIDE (every sibling on the same key hits the identical wall at the
identical moment) and TEMPORAL (the child's session and work are intact and
re-runnable once the provider's clock rolls over). A parent that reads it as
an ordinary failure respawns a replacement into the same wall, which a live
incident measured. Three layers carry it:

- The ADAPTER classifies, never the engine. `provider/anthropic`'s
  `parseUsageExhaustion` gates on the HTTP status (400/402/403/429, or none
  at all for a mid-stream `error` event) and then matches
  `usageExhaustionPatterns` — a deliberately flat, extensible list of
  observed wall wordings, one regexp per shape, each of which must name a
  spent SUPPLY, never a per-minute THROTTLE. It returns a
  `provider.Error{Kind: ErrKindProviderExhausted, RecoverHint}` wrapped
  permanent (no backoff outlives a spent quota). This is the second place
  message matching is tolerated, under `parseContextOverflow`'s rules. Other
  adapters opt in by producing the same kind; only anthropic does today.
- The ENGINE reads the typed classification, never text.
  `classifySpawnFailure` (`engine/session_manager.go`) maps
  `provider.AsProviderExhausted` — or a `RetryableRateLimited` class that
  outlived the retry budget — to `FailKindProviderExhausted`
  (`"provider_exhausted"`). Overload and 5xx weather deliberately do NOT
  qualify: those clear in seconds and a sibling may well succeed.
- The STATUS VOCABULARY is unchanged. An exhausted child is `StatusFailed`,
  with the kind in a SEPARATE `FailKind` field (`SessionNode`,
  `taskNotification`, the durable `taskNotifyRecord`, `task status`'s
  `fail_kind`, `GET /session/{id}.lineage.fail_kind`, the journal's
  `task_fail_kind`). A sixth `SessionStatus` value would have forced every
  cancellation/Reap/delivery/restore switch to grow an arm that behaves
  exactly like `StatusFailed`; only the PARENT's next move differs.

The rate-limit arm conflates a spent quota with a per-minute throttle that
outlived the child's small `PromptRetries` budget, one-directionally and on
purpose: a missed wall makes a parent respawn into it (the incident), while
a false wall costs one deferred resume of an intact child, and a hintless
guidance names no waiting period. An adapter that classifies its own quota
shape never reaches that arm. Both the cause and the recover-at hint go through
`boundedProviderText` (mask, then cap), so model-visible provider text on
this surface has one rule, not one per field. The hint is stated in ONE
engine-authored place — `taskFailureGuidance`'s "after <hint>" — never in
the reason prefix as well: the hint is extracted FROM the provider message
the reason already quotes, so naming it there made one rendered line
repeat the same time three times. It rides the durable record
(`taskNotifyRecord.FailHint`) because that guidance clause is now the only
carrier of the fact.

`taskFailureGuidance` (`engine/taskdelivery.go`) appends the parent's
instructions to that child's own notification line — child preserved, do not
spawn a replacement, resume with `task send` on this session id, after the
recover-at hint when the provider gave one. Resuming is the existing
send-to-a-settled-descendant re-run path, unchanged. A turn that then
succeeds clears `failReason`/`failKind` on the node, so a resumed child
stops reporting a wall it already got past; `finalizeTurn`'s
`alreadyCanceled` branch clears them too, since a CANCELED re-run must not
keep snapshotting a classification no live cancellation sets and
`restoreKnownStatusLocked` restores as empty.

A REAPED descendant is still resolvable, not "no such session." `Reap`
collects a done/failed/canceled LEAF the instant it settles (its own doc
comment), which a caller that spawned it has no way to observe before
asking about it again — a live incident hit exactly this: `task send` to
a settled child answered `no such session` depending on internal reap
timing the parent could not see. `resolveOrReviveDescendantLocked`
(`engine/session_manager.go`), the shared first step of all four verbs,
falls back to disk when a live-tree lookup misses: `LoadSession` the
target, then confirm ancestry from its own DURABLE `task_parent_id`
chain (`durableAncestorChainHas`) — never from live state alone, which is
exactly what `Reap` already erased. Only a target with no session log on
disk either, or whose durable lineage does not reach the caller, still
answers `ErrUnknownSession`/`ErrNotDescendant`. The four verbs then
diverge on what a successful disk resolution does: `send` RE-ADOPTS the
revived child into the tree (`adoptReloadedLocked`, the same
adopt-on-first-sight path `AdoptReloaded`/`handleSpawnChild`'s
parent-lookup fallback already use) and re-runs it exactly like a
settled-but-unreaped child — `budgetedByChild` surviving `Reap` by design
is what stops that re-adopt from double-crediting its already-spent
usage. `status`/`log` serve the disk-loaded state directly
(`durableSnapshot`/`deriveSettledStatus`) WITHOUT re-adopting: a
read-only, poll-shaped verb must not have the side effect of pinning a
reaped descendant back into memory. `cancel` on a reaped target is a
no-op success (nothing left to interrupt) reporting its real terminal
status, never `StatusCanceled`. The disk-bound half of this resolution
runs with `m.mu` released — one slow disk read must never stall every
other session's `Info`/`Reap`/`Spawn`/`Send` call — and re-validates
`m.nodes[targetID]` on reacquiring the lock, so a concurrent adopt of the
same id (another `Spawn`, `AdoptReloaded`, or a second racing revival)
always wins single-handedly: whichever adoption reaches `m.nodes` first
is authoritative, and the loser's own "already managed" is ignored, the
same rule `AdoptReloaded`'s existing callers already follow for that
race.

A parent can read a dead child's tail. The `task` tool's `log` verb
(`runTaskLog`, `engine/task_tool.go`, over
`SessionManager.DescendantTranscript`) returns the last N transcript
entries of a descendant, LIVING OR DEAD, under the same ancestor gate
(`isDescendantLocked`, or the disk-backed lineage check above once
`Reap` has removed the live node) cancel/status/send use — a terminal
node keeps its `*Session`, history included, until `Reap`, so no reload
and no disk read is involved for a still-tracked descendant. It is
bounded on three axes, because its output lands in the
PARENT's context and replays on every later turn: `tail` (default 20,
clamped at 100, a negative value is an error), a per-entry rune cap, and a
total rune budget filled NEWEST-first so the messages nearest a death
always survive. The reply reports the descendant's whole message count
next to how many entries came back, so a model knows it is reading a
window, and it carries `fail_kind` alongside `fail_reason` — the same
structured half `task status` reports, so a reader with the tail in front
of it never needs a second call to learn a death was an account wall
rather than the child. Every non-text part is rendered rather than dropped — a tool call
with capped arguments, a tool result, a reasoning summary, and an
attachment COUNT that includes blobs nested inside a tool result, which
`Parts.Text()` itself drops. Content is deliberately NOT masked: parent
and child are the same operator's sessions in one process, and a child's
final text already reaches the parent verbatim in its completion
notification.

`Config.OnRequest` receives the firing session's own id as its first
parameter (`engine/engine.go`). Never wire it as a closure over a
captured session variable: `configSnapshot` copies the func value into
every spawned child, which misattributes the child's `request.meta`
records to the closed-over session's id.

**Hub spawn contract:** the hub that spawns boxes — `harness hub`, now
implemented in `tools/hub/` (see `docs/development-interfaces.md`) — passes the
generated box NAME to the spawn command's environment as
`HARNESS_HUB_BOX_NAME`, so deployment scripts can derive per-name storage
(e.g. mount/create a volume named after it) without the hub and the box
needing any other side channel to agree on identity. Harness itself never
reads this variable — it is a contract between the hub and deployment
tooling, documented in `docs/design/fleet-model.md` §8.

## Serve-mode latency diagnostics

A caller that waits seconds for a reply cannot tell, from outside the
process, whether `harness serve` was slow, the network in front of it was
slow, or the whole process was stopped by garbage collection. Three
threshold-gated WARN lines answer that, and nothing runs always-on.

- **`slow request`** (`server/timing.go`). `serveTimed` wraps the mux
  dispatch in `Server.ServeHTTP` and warns when this process took longer
  than `slowRequestThreshold` (500ms) to answer, with `method`, `route`,
  `status`, `duration_ms`, and the caller's `X-Request-Id` as
  `request_id`. The route is `http.Request.Pattern`, which the mux sets
  during the dispatch, so a session id never reaches a log line; a request
  that matched no route logs the fixed `unmatched` label, because the path
  is caller-controlled. `requestID` drops a header value over 64 bytes or
  carrying anything outside one printable ASCII token — it is untrusted
  input that lands in a log line. `longLivedRoutes` exempts `GET /event`
  and `GET /session/{id}/wait`: both run for as long as their caller
  wants, so timing them would warn for every healthy client. Keep that map
  in step with any new streaming or long-poll route.
- **`long gc pause`** (`cmd/harness/gcwatch.go`). A stop-the-world pause
  stops every goroutine, so the process logs nothing at all while it lasts
  and looks exactly like a wedged handler. `gcWatcher` samples the
  runtime's `/gc/pauses:seconds` histogram every 5 seconds and warns about
  a new pause at or past 200ms. It reads `runtime/metrics`, never
  `runtime.ReadMemStats` — `ReadMemStats` itself stops the world, so
  sampling it would add the pause this watcher exists to find.
  `longest_pause_ms` is the LOWER bound of the highest bucket that gained
  a pause, since a histogram records a range. The first sample reports
  nothing: the counts are cumulative for the whole process life. Same
  lifecycle as `inFlightWatchdog` — one cancelable context, cancelled when
  `serveCmd` returns.
- **`/debug/pprof/`** (`server/pprof.go`, `Options.PProf`, `harness serve
  -pprof`). OFF by default, and authed like every other route when on. It
  is the third step, not the first: `GET /debug/goroutines` needs no flag
  and already answers "what is this process blocked on". Turn `-pprof` on
  for a process under investigation when the CPU, heap, block, or mutex
  profile is what is missing.
  - **Never import `net/http/pprof` in this repository.** That package's
    `init` registers `/debug/pprof/*` on `http.DefaultServeMux` for the
    whole linked binary, so importing it — even to borrow its handler
    functions behind this flag — exposes profiling in ANY program that
    links `server` and serves the default mux
    (`http.ListenAndServe(addr, nil)`), with no opt-in and no way for
    `Options.PProf` to prevent it. Go runs a package's `init` on import;
    there is no way to take the handlers without the side effect. So
    `server/pprof.go` implements them on `runtime/pprof` and
    `runtime/trace` directly.
    `TestPProf_NotRegisteredOnDefaultServeMux` asserts the absence and is
    the regression guard: a 404 test through this server's own mux passes
    WITH the bad import and proves nothing. It exists TWICE, in `server/`
    and in `cmd/harness/`, because each only covers its own package's
    import graph — the binary links the engine, providers, plugins, MCP and
    the tools, and any one of them pulling in `net/http/pprof` would
    publish the endpoints for the whole process.
  - `?seconds=N` on `profile`/`trace` is clamped to 1-60 (default 30); a
    malformed or repeated value is a 400, through the same `intParam` every
    other integer parameter uses. A second concurrent CPU profile or trace
    is a 409, not a 500 — only one of each can run in a process — and the
    refusal removes the download headers it had to set before starting, so
    the error is JSON rather than a file a browser saves. A client
    disconnect ends the profile early rather than holding a runtime-wide
    lock for an abandoned request.
  - `GET /debug/pprof/{name}` is in `longLivedRoutes` (`server/timing.go`):
    a profile runs for exactly as long as the caller's `?seconds` asks, so
    timing it would log a 30-second "slow request" every time an operator
    ran `go tool pprof` against a box — their own tooling in the logs they
    are reading. The index stays timed; it returns at once.
  - The UNSLASHED `/debug/pprof` is registered explicitly, behind auth.
    Left to the mux it takes an automatic 308 redirect issued before any
    handler, which told an unauthenticated caller whether profiling is
    enabled. Authed, the two states are 401-vs-404 — the shape every other
    route in this API already has.
  - `/debug/pprof/symbol` is deliberately not served: `go tool pprof`
    symbolizes against the binary a profile came from.

No metrics, no tracing, no always-on profiling. A new diagnostic in this
area is a threshold-gated log line or it does not land.
