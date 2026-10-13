package plugin

import (
	"context"
	"encoding/json"
	"time"
)

// Plugin is a Harness plugin. A nil hook field is not subscribed. Every hook
// and tool function must be safe for concurrent use: the host overlaps calls,
// and one *Plugin can serve several runtimes, so package variables are shared.
// The context of a hook other than Event ends at the deadline of that hook. Do
// not change a Plugin after it passes to Serve or ServeConn.
type Plugin struct {
	Name    string // unique in a runtime
	Version string

	Event             func(ctx context.Context, c *Client, events []Event)                                        // async; batches may drop when the plugin is slow
	ChatParams        func(ctx context.Context, c *Client, req *ChatParamsRequest) (*ChatParamsResponse, error)   // a nil response changes nothing
	ChatMessage       func(ctx context.Context, c *Client, req *ChatMessageRequest) (*ChatMessageResponse, error) // the runtime does not dispatch chat.message
	SystemTransform   func(ctx context.Context, c *Client, req *SystemTransformRequest) (*SystemTransformResponse, error)
	ShellEnv          func(ctx context.Context, c *Client, req *ShellEnvRequest) (*ShellEnvResponse, error)
	ToolExecuteBefore *ToolBefore
	ToolExecuteAfter  func(ctx context.Context, c *Client, req *ToolExecuteAfterRequest) (*ToolExecuteAfterResponse, error)

	HookTimeouts map[Hook]time.Duration // per hook, 1 ms to 10 min in whole milliseconds, not Event; default 5 s

	Tools []Tool
}

// ToolBefore is the tool.execute.before hook of a plugin. A nil response from
// Run means no change.
type ToolBefore struct {
	DenyOnFailure bool // the host denies the call when this hook fails
	Run           func(ctx context.Context, c *Client, req *ToolExecuteBeforeRequest) (*ToolExecuteBeforeResponse, error)
}

// Tool pairs a tool definition with its implementation.
type Tool struct {
	Def     ToolDef
	Execute func(ctx context.Context, c *Client, args json.RawMessage) ([]Part, error) // an error becomes an error result for the model
}
