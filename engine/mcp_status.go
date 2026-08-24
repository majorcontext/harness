// Ambient degraded-MCP status block. Structurally mirrors
// engine/process.go's processStatusSegment (see that file's doc comment):
// computed fresh every streamTurn call from live state, appended only to
// the newest user message, never persisted to the session log. See
// docs/plans/2026-07-20-mcp-init-resilience.md Task 2 for the design.
package engine

import (
	"fmt"
	"strings"
)

// mcpStatusReader is implemented by an MCPRegistry that can also report
// live per-server status — *MCPManager satisfies it via Status (see
// mcp.go). This is a separate, narrower interface rather than a new method
// on MCPRegistry itself: MCPRegistry is a public contract that cmd/harness
// and server already build fakes against for their own tests, and growing
// it would force those out-of-scope-for-this-change packages to add a
// Status method too just to keep compiling. mcpStatusSegment treats "reg
// doesn't implement mcpStatusReader" exactly like "MCP not configured at
// all": nothing to report, "" every time — the same safe default a plain
// MCPRegistry fake already gets today.
type mcpStatusReader interface {
	Status() []MCPServerStatus
}

// mcpStatusSegment renders the ambient status block request assembly
// appends to the newest user message (see streamTurn): one clause per
// currently DEGRADED server — configured but not connected, because its
// first attempt failed and it is retrying in the background (or a
// subsequent retry has failed again) — sorted by name for deterministic
// output, matching MCPManager.Status's own ordering.
//
// Renders "" (absent — the zero happy-path cost the process-status block
// already commits to) in every one of these cases:
//   - reg is nil (no MCP configured at all)
//   - reg doesn't implement mcpStatusReader (a plain MCPRegistry fake with
//     no status surface — see mcpStatusReader's doc comment)
//   - reg's connect has never even been triggered yet, so Status returns
//     nil (see MCPManager.Status's doc comment on why "never asked" reads
//     as silence, not degradation, and is distinct from "asked and
//     everything's healthy")
//   - every configured server is currently connected
//
// This is computed fresh on every call — a mu-guarded map read plus string
// formatting — so it always reflects LIVE state: the moment a background
// retry commits a success, the very next call renders "" for that server,
// the same self-correcting property Task 1's retry state machine already
// gives Tools(). Nothing here is ever persisted (see streamTurn for the
// durability boundary this segment shares with the process one).
func mcpStatusSegment(reg MCPRegistry) string {
	if reg == nil {
		return ""
	}
	sr, ok := reg.(mcpStatusReader)
	if !ok {
		return ""
	}
	var tokens []string
	for _, st := range sr.Status() {
		if st.Connected {
			continue
		}
		tokens = append(tokens, formatMCPServerStatus(st))
	}
	if len(tokens) == 0 {
		return ""
	}
	return "[mcp: unavailable — " + strings.Join(tokens, ", ") +
		". Tools from these servers are temporarily absent and may return later in this session.]"
}

// formatMCPServerStatus renders one degraded server as "<name> (<reason>;
// retrying)" while its background retry is still active, or "<name>
// (<reason>; use the mcp tool action "connect" to retry)" once
// mcpRetryMaxAttempts has been exhausted and the entry is Parked (see
// retryServer) — either way a short error clause plus a posture the model
// can act on: still-retrying needs nothing from it, parked needs an
// explicit re-trigger. reason is always classifyMCPConnectError's output,
// never st.LastErr.Error() directly — this is model-visible context and a
// raw connect error can embed the server's endpoint URL (and any secret it
// carries); see classifyMCPConnectError's doc comment.
func formatMCPServerStatus(st MCPServerStatus) string {
	reason := classifyMCPConnectError(st.LastErr)
	if st.Parked {
		return fmt.Sprintf("%s (%s; use the mcp tool action %q to retry)", st.Name, reason, "connect")
	}
	return fmt.Sprintf("%s (%s; retrying)", st.Name, reason)
}
