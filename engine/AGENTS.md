# Engine

Read the root AGENTS.md. Behavior detail lives in `docs/`.

- Hold `Session.mu` while persistence and emission form one observation.
- Keep every STABLE system segment byte-stable in a session. Freeze live-state segments once.
- Keep the tool array byte-stable: built-ins by name, then MCP, then plugin.
- Return exactly one result per tool call, including on cancellation. Keep file-key order.
- Retry only typed retryable errors. Never retry cancellation or interrupted tool intent.
- Emit one `TurnMetrics` per completed provider call. Inject `Config.Now` in tests.
- Treat `StopMaxTokens` as incomplete. Continue within one `MaxTokensContinuations` budget.
- Keep the goal evaluator tool-less at `message.EffortOff`. Persist goal transitions.
- Treat the sidecar index and snapshots as caches. Refold or replay on doubt.
- Keep the prompt queue durable and FIFO. Persist enqueue before acceptance.
- Change model and effort only through `Session.SetModel` and `Session.SetEffort`.
- End summarization requests with a `RoleUser` instruction. Never prefill a folded assistant message.
- Keep first MCP connection lazy and bounded. Never defer schemas without the `mcp` selector.
- Persist child lineage. Preserve provider-exhausted children for resume.
- Prewarm emits no turn, message, usage, or provider event. The callback obeys cancellation.
- Inject only the Agent Skills catalog. Never persist it.
