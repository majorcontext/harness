package harness

import "github.com/majorcontext/harness/internal/testhook"

var _ = func() bool {
	testhook.Hooks.HoldMCPRetries = func(opts any) { opts.(*Options).mcpHoldRetries = true }
	testhook.Hooks.AwaitSessionMCP = func(opts any) { opts.(*Options).mcpAwaitSession = true }
	return true
}()
