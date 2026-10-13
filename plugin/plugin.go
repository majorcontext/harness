package plugin

import (
	"context"
	"encoding/json"
	"time"
)

// Plugin is a Harness plugin. A nil hook field is not subscribed. The package
// documentation describes each hook. Every hook and tool function must be safe
// for concurrent use. Do not change a Plugin after the host or Serve gets it.
type Plugin struct {
	Name    string // unique in a runtime; a process plugin uses the name of its config entry
	Version string

	Event             func(ctx context.Context, c *Client, events []Event)                                        // not waited for; a batch can drop when the plugin is slow
	ChatParams        func(ctx context.Context, c *Client, req *ChatParamsRequest) (*ChatParamsResponse, error)   // a nil response changes nothing
	ChatMessage       func(ctx context.Context, c *Client, req *ChatMessageRequest) (*ChatMessageResponse, error) // the runtime never calls it
	SystemTransform   func(ctx context.Context, c *Client, req *SystemTransformRequest) (*SystemTransformResponse, error)
	ShellEnv          func(ctx context.Context, c *Client, req *ShellEnvRequest) (*ShellEnvResponse, error)
	ToolExecuteBefore *ToolBefore
	ToolExecuteAfter  func(ctx context.Context, c *Client, req *ToolExecuteAfterRequest) (*ToolExecuteAfterResponse, error)

	HookTimeouts map[Hook]time.Duration // whole ms from 1 ms to 10 min, default 5 s; a key is a hook that is set, not HookEvent

	Tools []Tool // a name must not repeat another tool of the session
}

// ToolBefore is the tool.execute.before hook of a plugin. A nil response from
// Run means no change.
type ToolBefore struct {
	DenyOnFailure bool // a failure of this hook denies the call; set it for a guard
	Run           func(ctx context.Context, c *Client, req *ToolExecuteBeforeRequest) (*ToolExecuteBeforeResponse, error)
}

// Tool pairs a tool definition with its implementation. Execute returns
// PartText and PartBlob parts.
type Tool struct {
	Def     ToolDef
	Execute func(ctx context.Context, c *Client, args json.RawMessage) ([]Part, error) // an error becomes an error result with its text
}
