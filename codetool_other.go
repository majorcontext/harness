//go:build !js

package harness

import (
	"github.com/majorcontext/harness/internal/tool/codetool"
	"github.com/majorcontext/harness/internal/turn"
)

// newCodeTool returns the code tool, or nil while code_mode is not true.
func newCodeTool(opts Options) turn.Tool {
	if !opts.Config.CodeModeEnabled() {
		return nil
	}
	return codetool.New()
}
