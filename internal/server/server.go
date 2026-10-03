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

	"github.com/majorcontext/harness/protocol"
)

// Session is a session that the Runtime runs.
type Session interface {
	View() protocol.Session
	Admit(ctx context.Context, in protocol.Input) (receipt protocol.Admitted, repeat bool, err error)
	Interrupt(ctx context.Context, req protocol.Interrupt) error
	Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)
	Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
}

// Runtime hosts the sessions.
type Runtime[S Session] interface {
	Create(ctx context.Context, req protocol.CreateSession) (S, error)
	Open(ctx context.Context, id string) (S, error)
	List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error)
	Models() []protocol.Model
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
	protocol.CodeInvalidRequest:   http.StatusBadRequest,
	protocol.CodeSessionNotFound:  http.StatusNotFound,
	protocol.CodeSessionExists:    http.StatusConflict,
	protocol.CodeSessionNotOwned:  http.StatusConflict,
	protocol.CodeInputConflict:    http.StatusConflict,
	protocol.CodeTurnMismatch:     http.StatusConflict,
	protocol.CodeModelUnavailable: http.StatusConflict,
	protocol.CodePayloadTooLarge:  http.StatusRequestEntityTooLarge,
	protocol.CodeDraining:         http.StatusServiceUnavailable,
}

// errInvalid reports a request that the handler cannot decode.
var errInvalid = errors.New("invalid request")

type handler[S Session] struct {
	rt    Runtime[S]
	codes []Code
}

// New returns the HTTP API of rt. An error gets the code of the first entry
// of codes that it matches, or CodeInternal.
func New[S Session](rt Runtime[S], codes []Code) http.Handler {
	h := &handler[S]{rt: rt, codes: append([]Code{{errInvalid, protocol.CodeInvalidRequest}}, codes...)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", h.serve(h.create))
	mux.HandleFunc("GET /sessions", h.serve(h.list))
	mux.HandleFunc("GET /sessions/{id}", h.session(h.view))
	mux.HandleFunc("PATCH /sessions/{id}", h.session(h.update))
	mux.HandleFunc("POST /sessions/{id}/inputs", h.session(h.submit))
	mux.HandleFunc("POST /sessions/{id}/interrupt", h.session(h.interrupt))
	mux.HandleFunc("GET /sessions/{id}/events", h.session(h.events))
	mux.HandleFunc("GET /models", h.serve(func(w http.ResponseWriter, _ *http.Request) error {
		reply(w, http.StatusOK, rt.Models())
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
// a session that it already runs as is.
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

func (h *handler[S]) view(s S, w http.ResponseWriter, _ *http.Request) error {
	reply(w, http.StatusOK, s.View())
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

func (h *handler[S]) events(s S, w http.ResponseWriter, r *http.Request) error {
	after, err := number(r, "after")
	if err != nil {
		return err
	}
	if acceptsStream(r.Header.Get("Accept")) {
		if id := r.Header.Get("Last-Event-ID"); id != "" {
			if after, err = strconv.ParseUint(id, 10, 64); err != nil {
				return fmt.Errorf("%w: Last-Event-ID: %w", errInvalid, err)
			}
		}
		h.stream(s, w, r, after)
		return nil
	}
	n, err := limit(r)
	if err != nil {
		return err
	}
	head := s.View().HeadSeq
	page := protocol.EventPage{Events: []protocol.Event{}}
	if after < head {
		for e, err := range s.Events(r.Context(), after) {
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
