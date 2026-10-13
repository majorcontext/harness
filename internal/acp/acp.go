// Package acp serves the Agent Client Protocol over a byte stream. It knows
// nothing about its host: the caller supplies an io.Reader and an io.Writer.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/acp/rpc"
	"github.com/majorcontext/harness/protocol"
)

// shutdownBudget bounds the release of the sessions and the close of the
// runtimes at the end of a connection.
const shutdownBudget = 5 * time.Second

// LatestVersion is the newest ACP protocol version that the adapter speaks.
const LatestVersion = 1

// Config holds the values that the adapter needs from its host.
type Config struct {
	// Version is the harness version that initialize reports.
	Version string
	// Logger receives diagnostics. Nil discards them.
	Logger *slog.Logger
	// AllowedTools is the allowed_tools of every session that session/new
	// creates. Nil allows every tool.
	AllowedTools []string
	// MCP says that the host starts MCP servers. Without it the agent
	// advertises no MCP transport and refuses mcpServers.
	MCP bool
}

// Runtimes gives the adapter the Runtime of a work directory. The host builds
// one for each cwd, with the project config of that cwd, over one Store and one Owner.
type Runtimes interface {
	// For returns the Runtime of cwd, which is an absolute path. A cwd that is
	// not a directory fails with an error that matches harness.ErrInvalidRequest.
	For(ctx context.Context, cwd string) (Runtime, error)
	// Close closes every Runtime that For built.
	Close(ctx context.Context) error
}

// Runtime is what the adapter uses of a harness.Runtime.
type Runtime interface {
	Create(ctx context.Context, req protocol.CreateSession, opts ...harness.SessionOption) (*harness.Session, error)
}

// Serve answers ACP requests from r on w until r ends or ctx ends. Then it
// releases the sessions it holds and closes rts. A nil rts refuses session/new.
func Serve(ctx context.Context, rts Runtimes, r io.Reader, w io.Writer, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	conn := rpc.New(r, w, cfg.Logger)
	a := &agent{cfg: cfg, rts: rts, conn: conn, base: ctx, sessions: map[string]*held{}}
	err := conn.Serve(ctx, a)
	return errors.Join(err, a.closeErr)
}

type agent struct {
	cfg  Config
	rts  Runtimes
	conn *rpc.Conn
	base context.Context

	mu       sync.Mutex
	sessions map[string]*held
	closeErr error
}

func (a *agent) Request(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return a.initialize(params)
	case "session/new":
		return a.newSession(ctx, params)
	case "session/prompt":
		return a.prompt(ctx, params)
	}
	return nil, &rpc.Error{Code: rpc.CodeMethodNotFound, Message: "Method not found", Data: map[string]string{"method": method}}
}

func (a *agent) Notification(ctx context.Context, method string, params json.RawMessage) {
	if method != "session/cancel" {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	if h, err := a.held(p.SessionID); err == nil {
		h.cancel(a.base)
	}
}

// held returns the session of the connection, or the not-found error.
func (a *agent) held(id string) (*held, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if h, ok := a.sessions[id]; ok {
		return h, nil
	}
	return nil, rpcError(harness.ErrSessionNotFound)
}

// EndOfInput releases every held session, which ends the turns that run, then
// closes the runtimes, within shutdownBudget, also after the base context ended.
func (a *agent) EndOfInput() { a.closeErr = a.shutdown(a.base) }

func (a *agent) shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
	defer cancel()
	a.mu.Lock()
	sessions := a.sessions
	a.sessions = map[string]*held{}
	a.mu.Unlock()
	var errs []error
	for _, h := range sessions {
		if err := h.sess.Release(ctx); err != nil && !errors.Is(err, harness.ErrSessionNotOwned) {
			errs = append(errs, err)
		}
		h.close()
	}
	if a.rts != nil {
		errs = append(errs, a.rts.Close(ctx))
	}
	return errors.Join(errs...)
}

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
			"mcpCapabilities":     map[string]any{"http": a.cfg.MCP, "sse": false},
			"sessionCapabilities": map[string]any{},
		},
		"authMethods": []any{},
	}, nil
}
