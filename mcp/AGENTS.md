# MCP client

Read the root AGENTS.md. `engine/AGENTS.md` owns connection policy.

- Keep this package independent from engine, server, and command code.
- Keep JSON-RPC framing dependency-free. Preserve request ID correlation.
- Stdio uses one JSON-RPC message per line.
- Streamable HTTP accepts a JSON or SSE response. Preserve `MCP-Session-Id`.
- Preserve `tools/list` and `resources/list` pagination cursors.
- Send static request headers on every HTTP call.
- In scope: `tools/list`, `tools/call`, `resources/list`, `resources/read`.
- Add no OAuth, client capabilities, legacy HTTP+SSE, subscriptions, or prompts.
- Preserve text, image, audio, resource-link, embedded-resource, and `isError` fields.
- Never collapse structured content into text here.
- Pin framing and HTTP behavior with an `e2e/` contract row. Make no remote call. Test a transport directly only in a file that `testdata/test-exceptions.txt` lists.
