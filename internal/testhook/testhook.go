package testhook

// Hooks are test-only; production code never calls one. HoldMCPRetries and AwaitSessionMCP take a *harness.Options.
var Hooks struct {
	HoldMCPRetries  func(opts any)
	AwaitSessionMCP func(opts any)
}
