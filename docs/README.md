# Harness documentation

Use this index to find technical documentation. The current source and tests
are authoritative.

## Runtime behavior

| Document | Subject |
|---|---|
| [plugins-and-protocols.md](plugins-and-protocols.md) | Plugin lifecycle and external protocol boundaries |
| [fleet-and-serve.md](fleet-and-serve.md) | Fleet state, lineage, exhaustion, and diagnostics |
| [deploy-modal.md](deploy-modal.md) | Deployment modal behavior |

The plugin wire contract is in [internal/plugin/PROTOCOL.md](../internal/plugin/PROTOCOL.md).

## Designs

`design/` contains architectural designs and durable decisions. Keep current
behavior in the runtime documents above. Keep implementation history and
superseded chronology in the commit body and the pull request body, not in
a standalone document.

| Design | Subject |
|---|---|
| [architecture.md](architecture.md) | Target architecture and migration phases |
| [context-compaction.md](design/context-compaction.md) | Automatic and manual context compaction |
| [codex-websocket-chaining.md](design/codex-websocket-chaining.md) | Codex response chaining and startup prewarm |
| [fleet-model.md](design/fleet-model.md) | Task lineage, fleet state, and provider exhaustion |
| [git-changes.md](design/git-changes.md) | GET /git/changes: constant-cost diff computation without touching the index |
| [managed-processes.md](design/managed-processes.md) | Box-scoped managed process lifecycle |
| [mcp-lazy-tools.md](design/mcp-lazy-tools.md) | Deferred MCP schema design |
| [nested-instruction-loading.md](design/nested-instruction-loading.md) | Project instruction discovery and truncation |
| [slash-commands.md](design/slash-commands.md) | Human-invoked control and prompt commands in one registry |
