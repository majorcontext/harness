# End-to-end

Read the root AGENTS.md.

- Contract scenarios drive the real binary over HTTP. The ACP table (`acp_scenarios_test.go`) drives `harness acp` over stdio, and `acp.Serve` over pipes, and checks each frame against the pinned ACP schema.
- Each row runs on the serve binary, and with `HARNESS_E2E_RUNTIME=1` also on `harness.Runtime` in process, through `httpapi.Handler`. Both hosts call the routes of `httpapi.Handler`.
- Each row has one disposition in `runtime_rows_test.go`. A same or pending row has a golden under `testdata/contract`, a re-golden row has one under `testdata/runtime` only, and a deleted row has none. A change that alters a runtime result updates it.
- Every test calls `skipShort(t)`, directly or through `runScenarios`.
- Do not poll state that has an in-process notification seam.
- Wait on an SSE stream or a channel.
- Each scenario owns its binary process and temp dirs.
- Start a subprocess only when the process boundary is under test.
- Keep fixtures local and deterministic. Require no live provider credentials.
- Keep subprocess output on failure. Mask secret values.
- Every HTTP client of a host comes from `wireClient`, which checks each response (a CORS preflight excepted) and error against `protocol/openapi.json` and the Errors table of the spec. A host that serves only the read routes (`httpapi.ReadHandler`) uses `wireClientReadOnly`, which also requires its refusals: 405 for a route that changes something, 404 for any other route it does not serve.
