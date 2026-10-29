package testhook

// Hooks are test-only; production code never calls one. HoldMCPRetries takes a *harness.Options.
var Hooks struct {
	HoldMCPRetries func(opts any)
}
