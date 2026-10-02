# Server

Read the root AGENTS.md. Read `engine/AGENTS.md` for session state machines.

- Update `server/openapi.yaml` with every API contract change.
- Resolve a live session only through `Server.resolveLive`. Read one `liveSession` snapshot.
- Never hold `server.mu` while acquiring `SessionManager.mu`.
- The durable journal is the source of truth. Index and snapshot are caches.
- Keep the unparameterized message endpoint response unchanged. Add no orphan repairs to a page.
- Return `started` only for the prompt that received the run slot.
- Never let a fresh prompt jump a restored queue head. Never dispatch a restored queue at boot.
- Dispatch queued input before goal auto-arm. Keep abort independent from queue clear.
- Keep `POST /enqueue` write-ahead and idempotent.
- Validate attachments before the run slot is claimed. Share `decodePromptParts` across endpoints.
- Add an attachment media type only when every adapter transcodes it.
- A worker park keeps the goal active. Context overflow clears it.
- Merge live and durable child IDs through one path. Mask and bound `fail_reason`.
- Fail closed unless the CLI selects an allowed unauthenticated mode. Keep `/health` open.
- Apply CORS only from configured origins.
- Never import `net/http/pprof`; `cmd/harness/pprof_defaultmux_test.go` guards the default mux. Keep pprof off by default and authenticated.
- Log the mux route pattern, never the raw path. Bound `X-Request-Id`.
- No ACP adapter exists. Do not describe one as implemented.
- Keep lock-order tests for each new lock edge.
