# Server instructions

These rules apply to `server/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root.
Read `engine/AGENTS.md` for session state machines.

## Layering

The server exposes the headless engine through HTTP and SSE. It must not import
`tools/*`. The CLI injects embedded tool pages through
`server.Options`.

Update `server/openapi.yaml` with an API contract change.

## Live session resolution

Use `Server.resolveLive` as the single resolution entry point for a live
session, status, or lineage. Read all values from one `liveSession` snapshot.

Do not hold `server.mu` while you acquire `SessionManager.mu`. Residency is
authoritative for the session's own running state. The manager supplies child
state that residency cannot.

## Journal, index, and cold reads

Durable journal records are the source of truth. An index or snapshot is a
cache.

- Use the session index for cold metadata only when its validation passes.
- Fall back to `LoadSession` for incomplete legacy metadata.
- Keep record folds shared with engine replay.
- Serve parameterized message pages from the durable sequence.
- Keep the unparameterized message endpoint response unchanged.
- Do not synthesize orphan results into a read-only transcript view.

Read `docs/design/journal-snapshotting.md` and
`docs/session-storage-and-queue.md` before changing these
paths.

## Prompt queue

`prompt_async` accepts same-session busy work into a durable FIFO queue.
Another session that owns the workdir still conflicts.

- Return `started` only for the prompt that received the run slot.
- Do not let a fresh prompt jump ahead of a restored queue head.
- Never auto-dispatch a restored queue during boot. Leave it for the next
  natural drain trigger.
- Dispatch queued input before goal auto-arm.
- Keep abort independent from queue clear.
- Keep `POST /enqueue` write-ahead and idempotent. Keep it text-only.
- Keep `GET /queue` as the reconciliation surface.

## Prompt attachments

`prompt_async` takes file blob parts beside its text parts: images and PDFs
today. Validate every attachment before the run slot is claimed: allowed
media type, inline data, bytes that really are the claimed type, and the
size cap. Reject in the handler. Never persist an attachment a provider
cannot render.

Add a media type only when EVERY provider adapter transcodes it. Each
accepted type owns a verifier in `promptAttachmentTypes`.

## Goal and turn state

Map a worker park to a distinct terminal outcome and keep the goal active.
Map context overflow to a clear, not a park.

A paused goal must not force the composite session idle while a real turn runs.
Activity can re-arm a parked goal only through the existing auto-arm path.
Resume needs no new mechanism: completion of an ordinary prompt uses that
same auto-arm path.

Log classified reasons. Do not journal raw provider errors or secrets.

`POST /session/{id}/thinking` parses the effort, accepts an empty string as
`EffortUnset`, and calls `Session.SetEffort` without claiming the run slot.

## Session lineage and fleet state

Read `docs/design/fleet-model.md` before changing box identity, session
lineage, revival, or goal pause behavior.

Merge live and durable child IDs through one de-duplicating path. Resolve a
reaped descendant from durable lineage before you report it missing.

Use `fail_kind` for machine behavior and bounded, masked `fail_reason` for
operator context.

Treat `provider_exhausted` as a recoverable account wall. Preserve the child
and guide the parent to resume it. Do not present the condition as a reason to
spawn a replacement.

## Authentication and browser surfaces

Fail closed unless the CLI explicitly selects an allowed unauthenticated mode.
Do not infer unauthenticated service from an empty token inside `server.New`.

Keep monitor HTML unauthenticated because it contains no secret. Keep API
routes under the normal auth policy. Apply CORS only from configured origins.

## Session monitor

`GET /monitor` serves bytes supplied through `Options.MonitorPage`.
`GET /{$}` redirects only when that page exists. Do not add a catch-all
route.

Keep the embedded page's CSP same-origin. Cross-origin monitoring uses a
separately hosted page.

Read `tools/AGENTS.md` before changing monitor behavior.

## Serve-mode latency diagnostics

A diagnostic must be opt-in or threshold-gated.

- Exclude streaming and long-poll routes from slow-request warnings.
- Log the mux route pattern, never the caller-controlled path.
- Bound and validate `X-Request-Id` before logging it.
- Keep pprof disabled by default and authenticated when enabled.
- Never import `net/http/pprof`. Its `init` mutates the default mux.
- Register the unslashed pprof path explicitly behind auth.
- Keep profile and trace durations bounded.
- Do not warn that a requested profile is a slow request.

The tests in both `server/` and `cmd/harness/` must prove that the default
mux has no pprof routes.

## External protocols

The current implementation exposes Harness HTTP/SSE and MCP-related surfaces.
ACP naming can guide a future adapter, but no ACP adapter currently exists.
Do not describe ACP as implemented until routes and tests exist.

A2A remains a non-goal.

If telemetry lands, use OpenTelemetry GenAI semantic conventions and standard
`OTEL_*` environment variables.

## Concurrency tests

Use `testing/synctest` for in-process timeout logic. Use notification seams
for status and queue transitions. Do not poll server state or add guessed
deadline wrappers.

Keep lock-order tests when a change introduces a new lock edge.
