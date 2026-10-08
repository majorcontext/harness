package harness

import (
	"context"

	"github.com/majorcontext/harness/protocol"
)

// Tool is an embedder tool that the model can call. Run allows concurrent
// calls; a Tool may add Alone() or Key(protocol.ToolCall) string to limit them.
type Tool interface {
	Spec() protocol.ToolSpec
	// Run receives call.ID, stable across a resumed turn. An error becomes an error result.
	Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error)
}
