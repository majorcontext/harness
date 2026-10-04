// Package server maps the Go API of a harness Runtime to HTTP and SSE.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/protocol"
)

// Session is a session that the Runtime runs.
type Session interface {
	View() protocol.Session
	Admit(ctx context.Context, in protocol.Input) (receipt protocol.Admitted, repeat bool, err error)
	Interrupt(ctx context.Context, req protocol.Interrupt) error
	Compact(ctx context.Context, req protocol.Compact) (protocol.Compacted, error)
	Resolve(ctx context.Context, requestID string, res protocol.Resolution) error
	SetGoal(ctx context.Context, g protocol.Goal) error
	ClearGoal(ctx context.Context) error
	Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)
	Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
}

// Reader is a session for reads: its state and its events, with no owner.
type Reader interface {
	Session() protocol.Session
	// Events yields the events after seq, and ends at the head when the
	// session is not running.
	Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
}

// Runtime hosts the sessions. A route that only reads a session calls Read,
// which owns nothing. A route that changes a session, or tails its events as
// they happen, calls Open.
type Runtime[S Session] interface {
	Create(ctx context.Context, req protocol.CreateSession) (S, error)
	Open(ctx context.Context, id string) (S, error)
	Read(ctx context.Context, id string) (Reader, error)
	List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error)
	Models() []protocol.Model
	Commands() (protocol.Commands, error)
}

// Processes runs the processes of the box.
type Processes interface {
	List() []process.Info
	Start(ctx context.Context, name string) (process.Status, error)
	Stop(ctx context.Context, name string) (process.Status, error)
	Restart(ctx context.Context, name string) (process.Status, error)
	Logs(name string, tail int) (string, process.Status, error)
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
}

// Code is the wire code of a sentinel error.
type Code struct {
	Err  error
	Code string
}

const (
	maxBody      = 8 << 20
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
}

// errInvalid reports a request that the handler cannot decode.
var errInvalid = errors.New("invalid request")

type handler[S Session] struct {
	rt      Runtime[S]
	codes   []Code
	workDir string
	procs   Processes
	// routes names the route that runs each control command.
	routes map[command.Op]route
}

type route struct{ method, path string }

// handle registers f at pattern, "METHOD path", as the route of each op.
func (h *handler[S]) handle(mux *http.ServeMux, pattern string, f http.HandlerFunc, ops ...command.Op) {
	mux.HandleFunc(pattern, f)
	method, path, _ := strings.Cut(pattern, " ")
	for _, op := range ops {
		h.routes[op] = route{method, path}
	}
}

// New returns the HTTP API of rt.
func New[S Session](rt Runtime[S], opts Options) http.Handler {
	h := &handler[S]{rt: rt, workDir: opts.WorkDir, procs: opts.Processes, routes: map[command.Op]route{}, codes: append([]Code{{errInvalid, protocol.CodeInvalidRequest},
		{process.ErrUnknownProcess, protocol.CodeProcessNotFound}, {workspace.ErrInvalid, protocol.CodeInvalidRequest},
		{workspace.ErrNotRepo, protocol.CodeNotAGitRepo}, {workspace.ErrNoBase, protocol.CodeNoBase},
		{workspace.ErrTooManyChanges, protocol.CodeTooManyChanges}}, opts.Codes...)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", h.serve(h.create))
	mux.HandleFunc("GET /sessions", h.serve(h.list))
	h.handle(mux, "GET /sessions/{id}", h.serve(h.view), command.OpStatus)
	h.handle(mux, "PATCH /sessions/{id}", h.session(h.update), command.OpSetModel, command.OpSetThinking, command.OpSetServiceTier)
	mux.HandleFunc("POST /sessions/{id}/inputs", h.session(h.submit))
	h.handle(mux, "POST /sessions/{id}/interrupt", h.session(h.interrupt), command.OpAbort)
	h.handle(mux, "POST /sessions/{id}/compact", h.session(h.compact), command.OpCompact)
	mux.HandleFunc("POST /sessions/{id}/requests/{request}", h.session(h.resolve))
	h.handle(mux, "PUT /sessions/{id}/goal", h.session(h.setGoal), command.OpSetGoal)
	h.handle(mux, "DELETE /sessions/{id}/goal", h.session(h.clearGoal), command.OpClearGoal)
	mux.HandleFunc("GET /sessions/{id}/events", h.serve(h.events))
	mux.HandleFunc("GET /models", h.serve(func(w http.ResponseWriter, _ *http.Request) error {
		reply(w, http.StatusOK, rt.Models())
		return nil
	}))
	h.box(mux)
	mux.HandleFunc("GET /commands", h.serve(func(w http.ResponseWriter, _ *http.Request) error {
		c, err := rt.Commands()
		if err != nil {
			return err
		}
		for i, e := range c.Commands {
			at := h.routes[command.Op(e.Op)]
			c.Commands[i].Method, c.Commands[i].Path = at.method, at.path
		}
		reply(w, http.StatusOK, c)
		return nil
	}))
	mux.HandleFunc("GET /health", h.serve(func(w http.ResponseWriter, _ *http.Request) error {
		reply(w, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))
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
		if allow := miss.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		reply(w, miss.status, errorBody(protocol.CodeInvalidRequest, errors.New(http.StatusText(miss.status))))
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

func (h *handler[S]) serve(f func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := f(w, r); err != nil {
			code := h.code(err)
			status, ok := statuses[code]
			if !ok {
				status = http.StatusInternalServerError
			}
			reply(w, status, errorBody(code, err))
		}
	}
}

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

func (h *handler[S]) code(err error) string {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return protocol.CodePayloadTooLarge
	}
	for _, c := range h.codes {
		if errors.Is(err, c.Err) {
			return c.Code
		}
	}
	return protocol.CodeInternal
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

// decode reads the JSON body into v. An empty body leaves v as is.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
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

func limit(r *http.Request) (int, error) {
	n, err := number(r, "limit")
	if n == 0 {
		return defaultLimit, err
	}
	return int(min(n, maxLimit)), err
}

func (h *handler[S]) create(w http.ResponseWriter, r *http.Request) error {
	var req protocol.CreateSession
	if err := decode(w, r, &req); err != nil {
		return err
	}
	s, err := h.rt.Create(r.Context(), req)
	if err != nil {
		return err
	}
	reply(w, http.StatusCreated, s.View())
	return nil
}

func (h *handler[S]) list(w http.ResponseWriter, r *http.Request) error {
	n, err := limit(r)
	if err != nil {
		return err
	}
	page, err := h.rt.List(r.Context(), protocol.ListSessions{After: r.URL.Query().Get("after"), Limit: n})
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, page)
	return nil
}

func (h *handler[S]) view(w http.ResponseWriter, r *http.Request) error {
	rd, err := h.rt.Read(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, rd.Session())
	return nil
}

func (h *handler[S]) update(s S, w http.ResponseWriter, r *http.Request) error {
	var p protocol.SettingsPatch
	if err := decode(w, r, &p); err != nil {
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
	if err := decode(w, r, &in); err != nil {
		return err
	}
	a, repeat, err := s.Admit(r.Context(), in)
	if err != nil {
		return err
	}
	if repeat {
		reply(w, http.StatusOK, a)
		return nil
	}
	reply(w, http.StatusCreated, a)
	return nil
}

func (h *handler[S]) interrupt(s S, w http.ResponseWriter, r *http.Request) error {
	var req protocol.Interrupt
	if err := decode(w, r, &req); err != nil {
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
	if err := decode(w, r, &req); err != nil {
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
	if err := decode(w, r, &res); err != nil {
		return err
	}
	if err := s.Resolve(r.Context(), r.PathValue("request"), res); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handler[S]) setGoal(s S, w http.ResponseWriter, r *http.Request) error {
	var g protocol.Goal
	if err := decode(w, r, &g); err != nil {
		return err
	}
	if err := s.SetGoal(r.Context(), g); err != nil {
		return err
	}
	reply(w, http.StatusOK, s.View())
	return nil
}

func (h *handler[S]) clearGoal(s S, w http.ResponseWriter, r *http.Request) error {
	if err := s.ClearGoal(r.Context()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// acceptsStream reports whether an Accept header lists text/event-stream
// with a quality above 0.
func acceptsStream(accept string) bool {
	for _, rng := range strings.Split(accept, ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(rng))
		if err != nil || media != "text/event-stream" {
			continue
		}
		if q, err := strconv.ParseFloat(params["q"], 64); params["q"] == "" || err != nil || q > 0 {
			return true
		}
	}
	return false
}

// events serves a page of events from a reader, and a stream of them from
// the opened session, which tails the events as they happen.
func (h *handler[S]) events(w http.ResponseWriter, r *http.Request) error {
	after, err := number(r, "after")
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	if acceptsStream(r.Header.Get("Accept")) {
		if last := r.Header.Get("Last-Event-ID"); last != "" {
			if after, err = strconv.ParseUint(last, 10, 64); err != nil {
				return fmt.Errorf("%w: Last-Event-ID: %w", errInvalid, err)
			}
		}
		s, err := h.rt.Open(r.Context(), id)
		if err != nil {
			return err
		}
		h.stream(s, w, r, after)
		return nil
	}
	n, err := limit(r)
	if err != nil {
		return err
	}
	rd, err := h.rt.Read(r.Context(), id)
	if err != nil {
		return err
	}
	head := rd.Session().HeadSeq
	page := protocol.EventPage{Events: []protocol.Event{}}
	if after < head {
		for e, err := range rd.Events(r.Context(), after) {
			if err != nil {
				return err
			}
			if !e.Ephemeral {
				page.Events = append(page.Events, e)
			}
			if e.Seq >= head || len(page.Events) == n {
				break
			}
		}
	}
	if last := len(page.Events) - 1; last >= 0 && page.Events[last].Seq < head {
		page.Next = page.Events[last].Seq
	}
	reply(w, http.StatusOK, page)
	return nil
}

// stream writes each event after seq after as an SSE frame. A durable frame
// carries its seq as the SSE id. An error ends the stream with an error frame.
func (h *handler[S]) stream(s S, w http.ResponseWriter, r *http.Request, after uint64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if rc.Flush() != nil {
		return
	}
	for e, err := range s.Events(r.Context(), after) {
		if err != nil {
			if r.Context().Err() == nil {
				data, _ := json.Marshal(errorBody(h.code(err), err))
				_ = frame(rc, w, "event: error\ndata: %s\n\n", data)
			}
			return
		}
		data, err := json.Marshal(e)
		if err != nil {
			return
		}
		id := ""
		if !e.Ephemeral {
			id = fmt.Sprintf("id: %d\n", e.Seq)
		}
		if frame(rc, w, "%sdata: %s\n\n", id, data) != nil {
			return
		}
	}
}

func frame(rc *http.ResponseController, w io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		return err
	}
	return rc.Flush()
}
