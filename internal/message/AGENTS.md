# Message

Read the root AGENTS.md. Read `internal/provider/AGENTS.md` for wire adapters.

- Opaque provider data carries a family tag. A different family drops it at transcode.
- Keep tool-call IDs provider-neutral. Adapters own wire-ID mapping.
- Keep `NoToolOutputText`, `ToolResult.SafeContent`, and `ToolResult.MarshalJSON` safe for empty output.
- Add a regression test for each new serializer or transcoder path.
- `NormalizeForWire` reshapes a throwaway request. It never deletes a real `ToolResult`.
- Keep wire-only shapes supported: duplicate call IDs, a call outside an assistant message, a result before its call, a same-role run between call and result.
- Keep relocation within `computeRelocationBarrier`.
- Derive `wire_oracle_test.go` from the provider contract, not an implementation.
- `EngineContext` is distinct from `Text`.
- `NeutralizeEngineContextSentinel` defangs the sentinel in user text.
- Never persist `EngineContext` as ambient status.
- Keep `Message.Normalize` pointer-stable for in-place cleanup.
- Test round trips for every part variant. Assert real tool output is never lost.
