//go:build !js

package harness

import (
	"github.com/majorcontext/harness/internal/tool/webfetch"
	"github.com/majorcontext/harness/internal/turn"
)

// webOptions configure the web_fetch tool of the sessions of a runtime.
type webOptions = webfetch.Options

// newWebOptions returns the options of web_fetch, or nil while web.fetch is not
// true or the host has no direct network.
func newWebOptions(opts Options) *webOptions {
	if !opts.Config.WebFetchEnabled() || opts.Host.NoNetwork {
		return nil
	}
	return &webOptions{Version: opts.Version, AllowLoopback: opts.WebAllowLoopback, Getenv: opts.Getenv, Resolver: opts.WebResolver}
}

func newWebTool(o webOptions) turn.Tool { return webfetch.New(o) }
