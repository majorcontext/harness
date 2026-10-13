package acp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/majorcontext/harness/internal/acp/rpc"
)

// Method is one authentication method that the host offers.
type Method struct {
	ID, Name, Description string
	// Terminal makes the client run the agent program with Args, in an interactive terminal.
	Terminal bool
	Args     []string
}

// Auth is the login that the host offers through ACP. The adapter calls it on first use and never at startup.
type Auth interface {
	// Methods returns the methods to advertise. It returns a method with Terminal set only when terminal is true.
	Methods(terminal bool) []Method
	// Authenticate runs the login of the agent method id. It stops when ctx ends.
	Authenticate(ctx context.Context, id string) error
	// Logout ends every login that Methods offers.
	Logout(ctx context.Context) error
	// Check fails with an *AuthRequiredError when a session of the work directory cwd needs a login that does not exist.
	// An empty model is the default model of cwd. fresh asks for a check that no cache answers.
	Check(ctx context.Context, cwd, model string, fresh bool) error
}

// AuthRequiredError says that a login is missing. Message tells the user what to do and holds no token.
type AuthRequiredError struct{ Message string }

func (e *AuthRequiredError) Error() string { return e.Message }

// codeAuthRequired is the JSON-RPC code of an operation that needs a login.
const codeAuthRequired = -32000

func (a *agent) methods() []Method {
	if a.cfg.Auth == nil {
		return nil
	}
	return a.cfg.Auth.Methods(a.terminalAuth.Load())
}

// authMethods is the authMethods member of the initialize answer.
func authMethods(methods []Method) []any {
	out := []any{}
	for _, m := range methods {
		wire := map[string]any{"id": m.ID, "name": m.Name, "description": m.Description}
		if m.Terminal {
			wire["type"] = "terminal"
			wire["args"] = m.Args
		}
		out = append(out, wire)
	}
	return out
}

func authCapabilities(methods []Method) map[string]any {
	if len(methods) == 0 {
		return nil
	}
	return map[string]any{"logout": map[string]any{}}
}

func notOffered(method string) error {
	return &rpc.Error{Code: rpc.CodeMethodNotFound, Message: "Method not found", Data: map[string]string{"method": method}}
}

func (a *agent) authenticate(ctx context.Context, params json.RawMessage) (any, error) {
	if len(a.methods()) == 0 {
		return nil, notOffered("authenticate")
	}
	var p struct {
		MethodID *string `json:"methodId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.MethodID == nil {
		return nil, invalidParams("methodId is required")
	}
	if !a.agentMethod(*p.MethodID) {
		return nil, invalidParams("methodId names no method of the agent type")
	}
	if err := a.cfg.Auth.Authenticate(ctx, *p.MethodID); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &rpc.Error{Code: rpc.CodeInternal, Message: err.Error()}
	}
	return map[string]any{}, nil
}

// agentMethod says whether id names an advertised method that authenticate runs.
func (a *agent) agentMethod(id string) bool {
	for _, m := range a.methods() {
		if m.ID == id && !m.Terminal {
			return true
		}
	}
	return false
}

func (a *agent) logout(ctx context.Context) (any, error) {
	if len(a.methods()) == 0 {
		return nil, notOffered("logout")
	}
	if err := a.cfg.Auth.Logout(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &rpc.Error{Code: rpc.CodeInternal, Message: err.Error()}
	}
	return map[string]any{}, nil
}

// requireLogin fails with the auth-required error when the model needs a login that does not exist.
func (a *agent) requireLogin(ctx context.Context, cwd, model string, fresh bool) error {
	if a.cfg.Auth == nil {
		return nil
	}
	if ae, ok := errors.AsType[*AuthRequiredError](a.cfg.Auth.Check(ctx, cwd, model, fresh)); ok {
		return authRequired(ae.Message)
	}
	return nil
}

func authRequired(msg string) error {
	return &rpc.Error{Code: codeAuthRequired, Message: msg, Data: map[string]string{"code": "login_required"}}
}
