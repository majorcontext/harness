# End-to-end

Read the root AGENTS.md.

- Contract scenarios drive the real binary over HTTP. They import no `engine` or `server` package.
- With `HARNESS_E2E_RUNTIME=1`, each row also runs on `harness.Runtime` in process, through `Runtime.Handler`.
- Each golden has one disposition in `runtime_rows_test.go`. A change that alters a runtime result updates it.
- Every test calls `skipShort(t)`, directly or through `runScenarios`.
- Do not poll state that has an in-process notification seam.
- Wait on `GET /session/{id}/wait`, an SSE stream, or a channel.
- Each scenario owns its binary process and temp dirs.
- Start a subprocess only when the process boundary is under test.
- Keep fixtures local and deterministic. Require no live provider credentials.
- Keep subprocess output on failure. Mask secret values.
