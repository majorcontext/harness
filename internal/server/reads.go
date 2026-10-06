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

	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/protocol"
)

// Blob is the bytes of an attachment, its media type, and the size that its
// part records.
type Blob struct {
	MediaType string
	Size      int
	Body      io.ReadCloser
}

// Reads is what the routes that only read need: the list of sessions, a
// reader for each, and the views that count the inputs that wait to run. The
// Runtime of an embedder with an owner implements it, and so does a Store with
// none.
type Reads interface {
	Read(ctx context.Context, id string) (Reader, error)
	// Session returns the state of session id, with the IDs of its queued inputs.
	Session(ctx context.Context, id string) (protocol.Session, error)
	List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error)
	// Inputs returns the queued inputs of session id, oldest first, never nil.
	Inputs(ctx context.Context, id string) ([]protocol.QueuedInput, error)
}

// readHandler serves the routes that read sessions.
type readHandler struct {
	reads Reads
	codes []Code
}

func newCodes(opts Options) []Code {
	return append([]Code{{errInvalid, protocol.CodeInvalidRequest},
		{process.ErrUnknownProcess, protocol.CodeProcessNotFound}, {workspace.ErrInvalid, protocol.CodeInvalidRequest},
		{workspace.ErrNotRepo, protocol.CodeNotAGitRepo}, {workspace.ErrNoBase, protocol.CodeNoBase},
		{workspace.ErrTooManyChanges, protocol.CodeTooManyChanges}}, opts.Codes...)
}

// NewReads returns the HTTP API of the routes of Table that only read, over
// rt. A route that changes something, and any other method of a read route,
// answers 405; any other path answers 404. The handler appends nothing, opens
// nothing, and starts no turn.
func NewReads(rt Reads, opts Options) http.Handler {
	h := &readHandler{reads: rt, codes: newCodes(opts)}
	mux := http.NewServeMux()
	bind(mux, h.readRoutes(h.eventsOf(func(ctx context.Context, id string, after uint64) (iter.Seq2[protocol.Event, error], error) {
		rd, err := rt.Read(ctx, id)
		if err != nil {
			return nil, err
		}
		return rd.Events(ctx, after), nil
	})), func(r Route) bool { return r.Read }, func(Route) {})
	refused, seen := http.NewServeMux(), map[string]bool{}
	for _, r := range Table {
		if pattern := r.Method + " " + r.Path; !r.Read && r.Method != http.MethodGet && !seen[pattern] {
			seen[pattern] = true
			refused.HandleFunc(pattern, http.NotFound)
		}
	}
	return envelope(mux, refused)
}

// readRoutes maps the name of each read route of Table to its handler. events
// serves both forms of the events route.
func (h *readHandler) readRoutes(events http.HandlerFunc) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"listSessions": h.serve(h.list),
		"getSession":   h.serve(h.view),
		"listInputs":   h.serve(h.queued),
		"listMessages": h.serve(h.messages),
		"listEvents":   events,
		"streamEvents": events,
		"getBlob":      h.serve(h.blob),
	}
}

func (h *readHandler) serve(f func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
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

func (h *readHandler) code(err error) string {
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

func (h *readHandler) list(w http.ResponseWriter, r *http.Request) error {
	n, err := limit(r)
	if err != nil {
		return err
	}
	page, err := h.reads.List(r.Context(), protocol.ListSessions{After: r.URL.Query().Get("after"), Limit: n})
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, page)
	return nil
}

func (h *readHandler) view(w http.ResponseWriter, r *http.Request) error {
	s, err := h.reads.Session(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, s)
	return nil
}

// messages answers a page of the conversation. It only reads the session.
func (h *readHandler) messages(w http.ResponseWriter, r *http.Request) error {
	if err := once(r, "before", "limit"); err != nil {
		return err
	}
	before, err := count(r, "before")
	if err != nil {
		return err
	}
	n, err := count(r, "limit")
	if err != nil {
		return err
	}
	if n > protocol.MaxMessageLimit {
		return fmt.Errorf("%w: limit must be at most %d", errInvalid, protocol.MaxMessageLimit)
	}
	rd, err := h.reads.Read(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	page, err := rd.Messages(r.Context(), uint64(before), n)
	if err != nil {
		return err
	}
	replyRaw(w, page)
	return nil
}

// queued answers the queued inputs with their parts, oldest first. It only
// reads the session.
func (h *readHandler) queued(w http.ResponseWriter, r *http.Request) error {
	in, err := h.reads.Inputs(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	replyRaw(w, in)
	return nil
}

// blob answers the bytes of an attachment of the session with its media type.
func (h *readHandler) blob(w http.ResponseWriter, r *http.Request) error {
	rd, err := h.reads.Read(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	b, err := rd.Blob(r.Context(), r.PathValue("key"))
	if err != nil {
		return err
	}
	defer func() { _ = b.Body.Close() }()
	w.Header().Set("Content-Type", b.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(b.Size))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, b.Body)
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

// eventsOf serves a page of events from a reader, and a stream of them from
// stream, which a runtime opens the session for and tails as events happen.
func (h *readHandler) eventsOf(stream func(ctx context.Context, id string, after uint64) (iter.Seq2[protocol.Event, error], error)) http.HandlerFunc {
	return h.serve(func(w http.ResponseWriter, r *http.Request) error {
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
			events, err := stream(r.Context(), id, after)
			if err != nil {
				return err
			}
			h.stream(events, w, r)
			return nil
		}
		return h.eventsPage(w, r, id, after)
	})
}

func (h *readHandler) eventsPage(w http.ResponseWriter, r *http.Request, id string, after uint64) error {
	n, err := limit(r)
	if err != nil {
		return err
	}
	rd, err := h.reads.Read(r.Context(), id)
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

// stream writes each of events as an SSE frame. A durable frame carries its
// seq as the SSE id. An error ends the stream with an error frame.
func (h *readHandler) stream(events iter.Seq2[protocol.Event, error], w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if rc.Flush() != nil {
		return
	}
	for e, err := range events {
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
