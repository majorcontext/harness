//go:build js

package harness

import "github.com/majorcontext/harness/internal/turn"

// webOptions is empty on js: the module links no web_fetch.
type webOptions struct{}

func newWebOptions(Options) *webOptions { return nil }

func newWebTool(webOptions) turn.Tool { return nil }
