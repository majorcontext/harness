// Package server maps the Go API of a harness Runtime to HTTP and SSE.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"strconv"

	"github.com/majorcontext/harness/internal/command"
	"github.com/majorcontext/harness/protocol"
)

// Session is a session that the Runtime runs.
type Session interface {
	View() protocol.Session
	Submit(ctx context.Context, in protocol.Input) (protocol.Admitted, error)
	Interrupt(ctx context.Context, req protocol.Interrupt) error
	Compact(ctx context.Context, req protocol.Compact) (protocol.Compacted, error)
	Resolve(ctx context.Context, requestID string, res protocol.Resolution) (protocol.Resolved, error)
	SetGoal(ctx context.Context, g protocol.Goal) error
	ClearGoal(ctx context.Context) error
	Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)
	Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
	// Withdraw removes input inputID from the queue. An input that is not
	// queued is no error.
	Withdraw(ctx context.Context, inputID string) error
}

// Reader is a session for reads: its state and its events, with no owner.
type Reader interface {
	Session() protocol.Session
	// Messages returns the page of the conversation before seq before.
	Messages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error)
	// Events yields the events after seq, and ends at the head when the
	// session is not running.
	Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
	// Inputs returns the queued inputs, oldest first, never nil.
	Inputs(ctx context.Context) ([]protocol.QueuedInput, error)
	// Blob returns the attachment that an input of the session names by key.
	Blob(ctx context.Context, key string) (Blob, error)
}

// Runtime hosts the sessions. A route that only reads a session calls Read,
// which owns nothing. A route that changes a session, or tails its events as
// they happen, calls Open.
type Runtime[S Session] interface {
	Reads
	Create(ctx context.Context, req protocol.CreateSession) (S, error)
	Open(ctx context.Context, id string) (S, error)
	End(ctx context.Context, id string) error
	Models() []protocol.Model
	Commands() (protocol.Commands, error)
}

// Processes runs the processes of the box.
type Processes interface {
	List() []protocol.ProcessInfo
	Start(ctx context.Context, name string) (protocol.ProcessStatus, error)
	Stop(ctx context.Context, name string) (protocol.ProcessStatus, error)
	Restart(ctx context.Context, name string) (protocol.ProcessStatus, error)
	Logs(name string, tail int) (protocol.ProcessLogs, error)
}

// Options configures the handler.
type Options struct {
	// Codes maps an error to the code of its first matching entry, or to
	// CodeInternal.
	Codes []Code
	// WorkDir is the root of GET /workspace/changes. Empty: no such route.
	WorkDir string
	// Processes serves the /processes routes. nil: no process runs.
	Processes Processes
	// Health is the body of GET /health.
	Health protocol.Health
}

// Code is the wire code of a sentinel error.
type Code struct {
	Err  error
	Code string
}

const (
	maxBody = 8 << 20
	// maxInputBody holds one attachment at its limit, as base64, and its text.
	maxInputBody = 32 << 20
	defaultLimit = 100
	maxLimit     = 1000
)

var statuses = map[string]int{
	protocol.CodeInvalidRequest:    http.StatusBadRequest,
	protocol.CodeSessionNotFound:   http.StatusNotFound,
	protocol.CodeSessionExists:     http.StatusConflict,
	protocol.CodeSessionNotOwned:   http.StatusConflict,
	protocol.CodeInputConflict:     http.StatusConflict,
	protocol.CodeTurnMismatch:      http.StatusConflict,
	protocol.CodeSessionBusy:       http.StatusConflict,
	protocol.CodeRequestNotPending: http.StatusConflict,
	protocol.CodeModelUnavailable:  http.StatusConflict,
	protocol.CodePayloadTooLarge:   http.StatusRequestEntityTooLarge,
	protocol.CodeDraining:          http.StatusServiceUnavailable,
	protocol.CodeNotAGitRepo:       http.StatusConflict,
	protocol.CodeNoBase:            http.StatusConflict,
	protocol.CodeTooManyChanges:    http.StatusConflict,
	protocol.CodeProcessNotFound:   http.StatusNotFound,
	protocol.CodeBlobNotFound:      http.StatusNotFound,
	protocol.CodeUnauthorized:      http.StatusUnauthorized,
	protocol.CodeInternal:          http.StatusInternalServerError,
}

// CodeStatuses returns the HTTP status of each error code of the API.
func CodeStatuses() map[string]int { return maps.Clone(statuses) }

// errInvalid reports a request that the handler cannot decode.
var errInvalid = errors.New("invalid request")

type handler[S Session] struct {
	readHandler
	rt      Runtime[S]
	workDir string
	procs   Processes
	health  protocol.Health
	// routes names the route that runs each control command.
	routes map[command.Op]route
}

type route struct{ method, path string }

// handlers maps the name of each route of Table to its handler.
func (h *handler[S]) handlers() map[string]http.HandlerFunc {
	m := h.readHandlers()
	maps.Copy(m, map[string]http.HandlerFunc{
		"createSession":    h.serve(h.create),
		"endSession":       h.serve(h.end),
		"updateSession":    h.session(h.update),
		"submitInput":      h.session(h.submit),
		"repeatInput":      h.session(h.submit),
		"withdrawInput":    h.session(h.withdraw),
		"interruptSession": h.session(h.interrupt),
		"compactSession":   h.session(h.compact),
		"resolveRequest":   h.session(h.resolve),
		"answerRequest":    h.session(h.resolve),
		"setGoal":          h.session(h.setGoal),
		"clearGoal":        h.session(h.clearGoal),
		"listModels": h.serve(func(w http.ResponseWriter, _ *http.Request) error {
			reply(w, http.StatusOK, h.rt.Models())
			return nil
		}),
		"listCommands": h.serve(h.commands),
		"health": h.serve(func(w http.ResponseWriter, _ *http.Request) error {
			reply(w, http.StatusOK, h.health)
			return nil
		}),
	})
	maps.Copy(m, h.boxHandlers())
	return m
}

// readHandlers are the handlers of the read routes. The stream of events opens
// the session, which tails its events as they happen.
func (h *handler[S]) readHandlers() map[string]http.HandlerFunc {
	return h.readRoutes(h.eventsOf(func(ctx context.Context, id string, after uint64) (iter.Seq2[protocol.Event, error], error) {
		s, err := h.rt.Open(ctx, id)
		if err != nil {
			return nil, err
		}
		return s.Events(ctx, after), nil
	}))
}

func (h *handler[S]) commands(w http.ResponseWriter, _ *http.Request) error {
	c, err := h.rt.Commands()
	if err != nil {
		return err
	}
	for i, e := range c.Commands {
		at := h.routes[command.Op(e.Op)]
		c.Commands[i].Method, c.Commands[i].Path = at.method, at.path
	}
	reply(w, http.StatusOK, c)
	return nil
}

// New returns the HTTP API of rt.
func New[S Session](rt Runtime[S], opts Options) http.Handler {
	h := &handler[S]{readHandler: readHandler{reads: rt, codes: newCodes(opts)}, rt: rt, workDir: opts.WorkDir, procs: opts.Processes, health: opts.Health, routes: map[command.Op]route{}}
	mux := http.NewServeMux()
	bind(mux, h.handlers(), func(r Route) bool { return !r.WorkDir || h.workDir != "" }, func(r Route) {
		for _, op := range r.Ops {
			h.routes[op] = route{r.Method, r.Path}
		}
	})
	return envelope(mux, nil)
}

// envelope answers a request that the mux does not route with the error body
// of the API: 404 for an unknown path and 405 for another method. A request
// that refused matches routes is 405 too, though the mux has no path for it.
func envelope(mux, refused *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		miss := &miss{header: http.Header{}}
		mux.ServeHTTP(miss, r)
		if miss.status != http.StatusNotFound && miss.status != http.StatusMethodNotAllowed {
			mux.ServeHTTP(w, r)
			return
		}
		status := miss.status
		if refused != nil {
			if _, pattern := refused.Handler(r); pattern != "" {
				status = http.StatusMethodNotAllowed
			}
		}
		if allow := miss.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		} else if status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", "")
		}
		reply(w, status, errorBody(protocol.CodeInvalidRequest, errors.New(http.StatusText(status))))
	})
}

// miss records the status and headers of a ServeMux routing error.
type miss struct {
	header http.Header
	status int
}

func (m *miss) Header() http.Header         { return m.header }
func (m *miss) Write(b []byte) (int, error) { return len(b), nil }
func (m *miss) WriteHeader(status int)      { m.status = status }

// session opens the session of the path through the Runtime, which returns
// a session that it already runs as is. Only a route that changes the
// session opens it.
func (h *handler[S]) session(f func(S, http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return h.serve(func(w http.ResponseWriter, r *http.Request) error {
		s, err := h.rt.Open(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		return f(s, w, r)
	})
}

// errorBody hides the cause of an internal error: it can hold store details.
func errorBody(code string, err error) protocol.ErrorBody {
	msg := err.Error()
	if code == protocol.CodeInternal {
		msg = "internal error"
	}
	return protocol.ErrorBody{Error: protocol.Error{Code: code, Message: msg, Details: map[string]any{}}}
}

// reply writes v. A write error means that the client is gone.
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// replyRaw is reply without HTML escapes, so a patch of HTML keeps its size.
func replyRaw(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// decode reads the JSON body of at most limit bytes into v. An empty body leaves v as is.
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return err
		}
		return fmt.Errorf("%w: %w", errInvalid, err)
	}
	return nil
}

// number parses the query parameter key, or returns 0 when it is absent.
func number(r *http.Request, key string) (uint64, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", errInvalid, key, err)
	}
	return n, nil
}

// count parses the query parameter key as a non-negative int, or returns 0
// when the request does not name it. A name with an empty value is an error.
func count(r *http.Request, key string) (int, error) {
	vs, ok := r.URL.Query()[key]
	if !ok {
		return 0, nil
	}
	n, err := strconv.Atoi(vs[0])
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: %s must be a non-negative integer", errInvalid, key)
	}
	return n, nil
}

// once rejects a query parameter that the request gives more than once.
func once(r *http.Request, keys ...string) error {
	q := r.URL.Query()
	for _, key := range keys {
		if len(q[key]) > 1 {
			return fmt.Errorf("%w: %s must be given at most once", errInvalid, key)
		}
	}
	return nil
}

func limit(r *http.Request) (int, error) {
	n, err := number(r, "limit")
	if n == 0 {
		return defaultLimit, err
	}
	return int(min(n, maxLimit)), err
}

func (h *handler[S]) create(w http.ResponseWriter, r *http.Request) error {
	var req protocol.CreateSession
	if err := decode(w, r, &req, maxBody); err != nil {
		return err
	}
	s, err := h.rt.Create(r.Context(), req)
	if err != nil {
		return err
	}
	reply(w, http.StatusCreated, s.View())
	return nil
}

func (h *handler[S]) update(s S, w http.ResponseWriter, r *http.Request) error {
	var p protocol.SettingsPatch
	if err := decode(w, r, &p, maxBody); err != nil {
		return err
	}
	v, err := s.Update(r.Context(), p)
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, v)
	return nil
}

// submit answers 201 for a new input and 200 for a repeat.
func (h *handler[S]) submit(s S, w http.ResponseWriter, r *http.Request) error {
	var in protocol.Input
	if err := decode(w, r, &in, maxInputBody); err != nil {
		return err
	}
	a, err := s.Submit(r.Context(), in)
	if err != nil {
		return err
	}
	if a.Repeat {
		reply(w, http.StatusOK, a)
		return nil
	}
	reply(w, http.StatusCreated, a)
	return nil
}

func (h *handler[S]) withdraw(s S, w http.ResponseWriter, r *http.Request) error {
	if err := s.Withdraw(r.Context(), r.PathValue("input")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handler[S]) interrupt(s S, w http.ResponseWriter, r *http.Request) error {
	var req protocol.Interrupt
	if err := decode(w, r, &req, maxBody); err != nil {
		return err
	}
	if err := s.Interrupt(r.Context(), req); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handler[S]) compact(s S, w http.ResponseWriter, r *http.Request) error {
	var req protocol.Compact
	if err := decode(w, r, &req, maxBody); err != nil {
		return err
	}
	c, err := s.Compact(r.Context(), req)
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, c)
	return nil
}

func (h *handler[S]) resolve(s S, w http.ResponseWriter, r *http.Request) error {
	var res protocol.Resolution
	if err := decode(w, r, &res, maxBody); err != nil {
		return err
	}
	got, err := s.Resolve(r.Context(), r.PathValue("request"), res)
	if err != nil {
		return err
	}
	if got.Status == protocol.ResolvedStarted {
		reply(w, http.StatusAccepted, got)
		return nil
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handler[S]) setGoal(s S, w http.ResponseWriter, r *http.Request) error {
	var g protocol.Goal
	if err := decode(w, r, &g, maxBody); err != nil {
		return err
	}
	if err := s.SetGoal(r.Context(), g); err != nil {
		return err
	}
	reply(w, http.StatusOK, s.View())
	return nil
}

func (h *handler[S]) end(w http.ResponseWriter, r *http.Request) error {
	if err := h.rt.End(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handler[S]) clearGoal(s S, w http.ResponseWriter, r *http.Request) error {
	if err := s.ClearGoal(r.Context()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
