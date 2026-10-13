// Package acp serves the Agent Client Protocol over a byte stream. It knows
// nothing about its host: the caller supplies an io.Reader and an io.Writer.
package acp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"

	"github.com/majorcontext/harness/internal/acp/rpc"
)

// LatestVersion is the newest ACP protocol version that the adapter speaks.
const LatestVersion = 1

// Config holds the values that the adapter needs from its host.
type Config struct {
	// Version is the harness version that initialize reports.
	Version string
	// Logger receives diagnostics. Nil discards them.
	Logger *slog.Logger
}

// Serve answers ACP requests from r on w until r ends or ctx ends.
func Serve(ctx context.Context, r io.Reader, w io.Writer, cfg Config) error {
	return rpc.New(r, w, cfg.Logger).Serve(ctx, &agent{cfg: cfg})
}

type agent struct{ cfg Config }

func (a *agent) Request(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method == "initialize" {
		return a.initialize(params)
	}
	return nil, &rpc.Error{Code: rpc.CodeMethodNotFound, Message: "Method not found", Data: map[string]string{"method": method}}
}

func (a *agent) Notification(context.Context, string, json.RawMessage) {}

func (a *agent) initialize(params json.RawMessage) (any, error) {
	var p struct {
		ProtocolVersion *int `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.ProtocolVersion == nil || *p.ProtocolVersion < 0 || *p.ProtocolVersion > 65535 {
		return nil, &rpc.Error{Code: rpc.CodeInvalidParams, Message: "Invalid params", Data: map[string]string{"reason": "protocolVersion must be an integer from 0 to 65535"}}
	}
	version := LatestVersion
	return map[string]any{
		"protocolVersion": version,
		"agentInfo":       map[string]any{"name": "harness", "title": "Harness", "version": a.cfg.Version},
		"agentCapabilities": map[string]any{
			"loadSession":         false,
			"mcpCapabilities":     map[string]any{"http": false, "sse": false},
			"sessionCapabilities": map[string]any{},
		},
		"authMethods": []any{},
	}, nil
}
