//go:build js

package harness

import "github.com/majorcontext/harness/internal/turn"

// newCodeTool returns nil on js: the module links no interpreter.
func newCodeTool(Options) turn.Tool { return nil }
