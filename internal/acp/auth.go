package acp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/acp/rpc"
)

// Method is one authentication method that the host offers.
type Method struct {
	ID, Name, Description string
	// Terminal makes the client run the agent program with Args, in an interactive terminal.
	Terminal bool
	Args     []string
	// Logout says that the logout method ends the login of this method, so the agent advertises logout.
	Logout bool
}

// ClientAuth is what the client of a connection can do for a login.
type ClientAuth struct {
	// Terminal says that the client runs a method of type terminal.
	Terminal bool
	// URL says that the client takes an elicitation in URL mode.
	URL bool
}

// LoginClient lets a login talk to the user through the client of the connection.
type LoginClient interface {
	// ElicitURL asks the client to send the user to url, tied to the authenticate request that ctx belongs to.
	ElicitURL(ctx context.Context, url, message string) (URLElicitation, error)
}

// ErrDeclined is the error of an elicitation that the user declined.
var ErrDeclined = errors.New("the sign-in was declined")

// URLElicitation is an elicitation in URL mode that the client has not completed.
type URLElicitation interface {
	// Accepted closes when the client accepts the elicitation, which says that the user opens the URL.
	Accepted() <-chan struct{}
	// Refused delivers one error when the client declines the elicitation (ErrDeclined), cancels it (context.Canceled), or fails it.
	Refused() <-chan error
	// Complete tells the client that the login ended.
	Complete()
	// Close withdraws the request when the client has not answered it.
	Close()
}

// Auth is the login that the host offers through ACP. The adapter calls it on first use and never at startup.
type Auth interface {
	// Methods returns the methods to advertise. It returns a method with Terminal set only when c.Terminal is true.
	Methods(c ClientAuth) []Method
	// Authenticate runs the login of the agent method id. It stops when ctx ends. client reaches the user.
	Authenticate(ctx context.Context, id string, client LoginClient) error
	// Logout ends the logins of the methods with Logout set.
	Logout(ctx context.Context) error
	// Check fails with an *AuthRequiredError when a session of the work directory cwd needs a login that does not exist.
	// An empty model is the default model of cwd. mode says which checks may run.
	Check(ctx context.Context, cwd, model string, mode CheckMode) error
}

// CheckMode says which checks Auth.Check may run.
type CheckMode int

const (
	// CheckCached is the check of a prompt: it reads only what earlier checks cached, and sends no probe.
	CheckCached CheckMode = iota
	// CheckNew is the check of a new session: every check runs, and a cache answers.
	CheckNew
	// CheckOpen is the check of a session that load or resume opens: a login that a probe tests is checked, and the other logins are not.
	CheckOpen
	// CheckFresh is the check after a failed turn: no cache answers.
	CheckFresh
)

// AuthRequiredError says that a login is missing. Message tells the user what to do and holds no token.
type AuthRequiredError struct{ Message string }

func (e *AuthRequiredError) Error() string { return e.Message }

// codeAuthRequired is the JSON-RPC code of an operation that needs a login.
const codeAuthRequired = -32000

func (a *agent) methods() []Method {
	if a.cfg.Auth == nil {
		return nil
	}
	return a.cfg.Auth.Methods(ClientAuth{Terminal: a.terminalAuth.Load(), URL: a.urlElicitation.Load()})
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
	if !logoutOffered(methods) {
		return nil
	}
	return map[string]any{"logout": map[string]any{}}
}

func logoutOffered(methods []Method) bool {
	return slices.ContainsFunc(methods, func(m Method) bool { return m.Logout })
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
	if err := a.cfg.Auth.Authenticate(ctx, *p.MethodID, loginClient{conn: a.conn, request: rpc.RequestID(ctx)}); err != nil {
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, ErrDeclined):
			return nil, &rpc.Error{Code: rpc.CodeInternal, Message: err.Error()}
		case errors.Is(err, context.Canceled):
			return nil, &rpc.Error{Code: rpc.CodeCancelled, Message: "Request cancelled"}
		case errors.Is(err, context.DeadlineExceeded):
			return nil, &rpc.Error{Code: rpc.CodeInternal, Message: "The sign-in timed out"}
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
	if !logoutOffered(a.methods()) {
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
func (a *agent) requireLogin(ctx context.Context, cwd, model string, mode CheckMode) error {
	if a.cfg.Auth == nil {
		return nil
	}
	if ae, ok := errors.AsType[*AuthRequiredError](a.cfg.Auth.Check(ctx, cwd, model, mode)); ok {
		return authRequired(ae.Message)
	}
	return nil
}

func authRequired(msg string) error {
	// The error text of a turn starts with the class of the failure in brackets; a client shows the rest.
	if rest, ok := strings.CutPrefix(msg, "["); ok {
		if _, text, ok := strings.Cut(rest, "] "); ok {
			msg = text
		}
	}
	return &rpc.Error{Code: codeAuthRequired, Message: msg, Data: map[string]string{"code": "login_required"}}
}

// loginClient implements LoginClient over the connection.
type loginClient struct {
	conn    *rpc.Conn
	request json.RawMessage
}

func (l loginClient) ElicitURL(_ context.Context, url, message string) (URLElicitation, error) {
	e := &urlElicitation{conn: l.conn, id: "elicit_" + newULID(), refused: make(chan error, 1), accepted: make(chan struct{})}
	pending, err := l.conn.Start("elicitation/create", map[string]any{"mode": "url", "requestId": l.request, "elicitationId": e.id, "url": url, "message": message})
	if err != nil {
		return nil, err
	}
	e.pending = pending
	go e.wait()
	return e, nil
}

type urlElicitation struct {
	conn     *rpc.Conn
	id       string
	pending  *rpc.Pending
	refused  chan error
	accepted chan struct{}
}

// wait reads the answer of the client. An accept says that the user opens the URL, and the login goes on.
func (e *urlElicitation) wait() {
	var res struct {
		Action string `json:"action"`
	}
	err := e.pending.Wait(context.Background(), &res)
	switch {
	case errors.Is(err, rpc.ErrCancelled):
	case err != nil:
		e.refused <- err
	case res.Action == "decline":
		e.refused <- ErrDeclined
	case res.Action == "cancel":
		e.refused <- context.Canceled
	default:
		close(e.accepted)
	}
}

func (e *urlElicitation) Accepted() <-chan struct{} { return e.accepted }

func (e *urlElicitation) Refused() <-chan error { return e.refused }

func (e *urlElicitation) Complete() {
	_ = e.conn.Notify("elicitation/complete", map[string]any{"elicitationId": e.id})
}

func (e *urlElicitation) Close() { e.pending.Cancel() }
