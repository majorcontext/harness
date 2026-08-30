// Package claudecode registers the provider family key that routes a
// message.ModelRef to harness's Claude Code CLI delegated-turn backend
// (engine/claude_code_backend.go, config.TypeClaudeCodeCLI).
//
// Client exists ONLY to satisfy provider.Registry lookups — the same map
// every native HTTP adapter (provider/anthropic, provider/openai, ...)
// registers into, which server.handleSetModel, the `model` tool, and
// Spawn's model-override validation all consult via Session.ModelSupported
// (engine/engine.go) before allowing a swap to a new ref. A claude-code
// session's actual turns NEVER reach Client.Stream in normal operation:
// PromptWithOrigin/runAgenticLoop (engine/engine.go) detect a claude-code
// model ref and dispatch to the delegated CLI driver BEFORE the native
// provider-call machinery (streamTurn) is ever reached — see that
// package's own doc comment for the full seam. Client.Stream is therefore
// a defensive backstop, not a real code path: if it is ever invoked, some
// caller bypassed that dispatch (a manual compaction call, a goal
// evaluator misconfigured to this family, a future call site that forgets
// the check), and returning a descriptive error here is far safer than
// either panicking or silently attempting an HTTP-shaped request this
// family was never meant to make.
package claudecode

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/provider"
)

// Family is the provider key clients register under and message.ModelRef.Provider
// values route by — "claude-code", matching config.TypeClaudeCodeCLI's
// conventional providers-map key and engine.ClaudeCodeProviderFamily. Kept
// as its own named constant (rather than importing engine, which would be
// a package-layering inversion: engine already imports provider) so
// cmd/harness's registry() has a single source for the string; a parity
// test in cmd/harness pins it against engine.ClaudeCodeProviderFamily.
const Family = "claude-code"

// Client is a provider.Provider stand-in for the claude-code family — see
// the package doc for why Stream is never expected to run.
type Client struct{}

// Name implements provider.Provider.
func (Client) Name() string { return Family }

// Stream implements provider.Provider. It always fails — see the package
// doc comment for why reaching this at all is itself the bug to fix, not a
// case this adapter should try to serve.
func (Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	return nil, fmt.Errorf("provider/claudecode: Stream called directly for model %s — a claude-code session's turns must be dispatched through the delegated CLI backend (engine/claude_code_backend.go), not the native provider-call path; this indicates a caller bypassed that dispatch", req.Model)
}
