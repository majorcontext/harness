package harness

import (
	"context"

	"github.com/majorcontext/harness/protocol"
)

// Tool is an embedder tool that the model can call.
type Tool interface {
	Spec() protocol.ToolSpec
	// Run receives call.ID, stable across a resumed turn, for idempotent
	// effects. An error becomes an error result that the model sees.
	Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error)
}
