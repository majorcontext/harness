# Message instructions

These rules apply to `message/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root. Read `provider/AGENTS.md` for wire
adapters and `engine/AGENTS.md` for live history.

## Canonical representation

Canonical messages are the durable representation. Do not add provider wire
objects to the message union.

Provider-specific opaque data uses a provider-family tag. The same family can
replay it. A different family drops it at transcode time.

Keep tool-call IDs provider-neutral in history. Adapters own deterministic
wire-ID mapping.

## Empty tool results

An empty tool result must not serialize as `null`.

- Keep `NoToolOutputText` as the canonical fallback.
- Keep `ToolResult.SafeContent` as the read path for consumers.
- Keep `ToolResult.MarshalJSON` safe for direct serialization.
- Add a regression test for each new serializer or transcoder path.

## Wire normalization

`ResolveOrphanToolCalls` runs on live or persisted state. It is additive-only.
It may add a synthetic result. It must not delete, move, or reorder a real part.

`NormalizeForWire` runs on a throwaway request. It may relocate data to meet
a provider contract. It must never delete a real `ToolResult`.

Keep support for these wire-only shapes:

1. Duplicate tool-call IDs in one assistant message.
2. A tool call in a non-assistant message.
3. A tool result before its call.
4. A same-role run between a call and its result.

Keep relocation within `computeRelocationBarrier`. Derive
`wire_oracle_test.go` from the provider contract, not either implementation.

Read the "Wire normalization" section in
`docs/engine-request-cycle.md` before changing this logic.

## EngineContext trust boundary

`EngineContext` is a structured part that only the engine creates. Keep it
distinct from `Text`.

`RenderEngineContext` wraps trusted context with the sentinel.
`NeutralizeEngineContextSentinel` defangs the same bytes in user text. Only a
real `EngineContext` may emit the trusted sentinel on the wire.

Keep the part in canonical JSON for runtime round trips. Do not turn it into a
persisted ambient-status mechanism.

## Normalization and mutation

When `Message.Normalize` sanitizes invalid tool arguments, preserve the
existing part pointer when callers rely on in-place cleanup before request
assembly.

Do not use a cleansing marshal as proof that resident state was clean. A
marshal can hide invalid in-memory input.

## Tests

- Use round-trip tests for every part variant.
- Use property tests for repair invariants.
- Assert that real tool output is never lost.
- Assert both missing and surplus tool results.
- Drive the real `LoadSession` or provider entry point when the defect occurs
  there.
