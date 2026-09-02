# Harness documentation

Use this index to find technical documentation. The current source and tests
remain authoritative when historical material describes an earlier behavior.

## Runtime behavior

| Document | Subject |
|---|---|
| [engine-request-cycle.md](engine-request-cycle.md) | Request assembly, file tools, retries, and metrics |
| [goal-loop.md](goal-loop.md) | Goal supervision and evaluator behavior |
| [session-storage-and-queue.md](session-storage-and-queue.md) | Indexes, snapshots, paging, queues, and processes |
| [models-and-providers.md](models-and-providers.md) | Model state, effort, cache affinity, and adapters |
| [mcp-tool-loading.md](mcp-tool-loading.md) | Deferred MCP schemas and stable tool ordering |
| [plugins-and-protocols.md](plugins-and-protocols.md) | Plugin lifecycle and external protocol boundaries |
| [development-interfaces.md](development-interfaces.md) | Hub behavior |
| [fleet-and-serve.md](fleet-and-serve.md) | Fleet state, lineage, exhaustion, and diagnostics |
| [deploy-modal.md](deploy-modal.md) | Deployment modal behavior |

The plugin wire contract is in [plugin/PROTOCOL.md](../plugin/PROTOCOL.md).
The goal-loop implementation history is in
[history/goal-loop-resilience.md](history/goal-loop-resilience.md).

## Designs and plans

`design/` contains architectural designs and durable decisions. `plans/`
contains dated implementation plans. Keep current behavior in the runtime
documents above and keep superseded chronology in `history/` or `plans/`.

| Design | Subject |
|---|---|
| [context-compaction.md](design/context-compaction.md) | Automatic and manual context compaction |
| [fleet-model.md](design/fleet-model.md) | Task lineage, fleet state, and provider exhaustion |
| [goal-retry-directive-reuse.md](design/goal-retry-directive-reuse.md) | Durable directive reuse across goal retries |
| [journal-snapshotting.md](design/journal-snapshotting.md) | Journal snapshot format and recovery |
| [managed-processes.md](design/managed-processes.md) | Box-scoped managed process lifecycle |
| [mcp-lazy-tools.md](design/mcp-lazy-tools.md) | Deferred MCP schema design |
| [nested-instruction-loading.md](design/nested-instruction-loading.md) | Project instruction discovery and truncation |
