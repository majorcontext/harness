package engine

import "context"

type toolCallIDKey struct{}

// ToolCallID returns the id of the tool call that ctx belongs to, or the
// empty string when ctx carries no tool call. The id is the one journaled in
// the assistant message, so a resumed turn that re-runs a call sees the same
// value.
func ToolCallID(ctx context.Context) string {
	id, _ := ctx.Value(toolCallIDKey{}).(string)
	return id
}
