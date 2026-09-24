package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// route is the serve-mode answer to one Op. A route is this dispatcher's
// mapping, not the operation: cmd/harness maps the same Op to a method
// call. See docs/design/slash-commands.md's "Serve-mode resolution"
// section.
type route struct {
	method string
	path   string
}

var opRoutes = map[command.Op]route{
	command.OpCompact:        {"POST", "/session/{id}/compact"},
	command.OpSetModel:       {"POST", "/session/{id}/model"},
	command.OpSetThinking:    {"POST", "/session/{id}/thinking"},
	command.OpSetServiceTier: {"POST", "/session/{id}/service-tier"},
	command.OpAbort:          {"POST", "/session/{id}/abort"},
	command.OpSetGoal:        {"POST", "/session/{id}/goal"},
	command.OpClearGoal:      {"DELETE", "/session/{id}/goal"},
	command.OpQueueList:      {"GET", "/session/{id}/queue"},
	command.OpQueueClear:     {"DELETE", "/session/{id}/queue"},
	command.OpStatus:         {"GET", "/session/{id}"},
	command.OpProcessList:    {"GET", "/process"},
}

// serveModeOps declares which control Ops serve mode resolves through
// its own routes, total over every command.Op: a new Op with no entry
// here fails TestServeModeOpsTotal instead of silently reaching a
// client as supported. queue-clear stays false: DELETE /session/{id}/queue
// already exists, but nothing dispatches it through serve mode yet. See
// docs/design/slash-commands.md's "Serve-mode resolution" section.
var serveModeOps = map[command.Op]bool{
	command.OpCompact:        true,
	command.OpSetModel:       true,
	command.OpSetThinking:    true,
	command.OpSetServiceTier: true,
	command.OpAbort:          true,
	command.OpSetGoal:        true,
	command.OpClearGoal:      true,
	command.OpQueueList:      true,
	command.OpQueueClear:     false,
	command.OpStatus:         true,
	command.OpProcessList:    true,
}

// serveUnsupportedReason is published verbatim to the person typing. It
// names no route and no internal term.
const serveUnsupportedReason = "Not available in this client."

// serveSupport reports whether serve mode resolves spec. A frontend
// command names no route: the frontend owns the session pointer, not
// serve mode.
func serveSupport(spec *command.Spec) (supported bool, reason string) {
	if spec.Kind == command.KindFrontend {
		return false, serveUnsupportedReason
	}
	if !serveModeOps[spec.Op] {
		return false, serveUnsupportedReason
	}
	return true, ""
}

type commandArgJSON struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
}

type commandEntryJSON struct {
	Name     string           `json:"name"`
	Aliases  []string         `json:"aliases,omitempty"`
	Kind     string           `json:"kind"`
	Op       string           `json:"op,omitempty"`
	Summary  string           `json:"summary"`
	ArgHint  string           `json:"arg_hint,omitempty"`
	Args     []commandArgJSON `json:"args,omitempty"`
	Category string           `json:"category"`
	// AvailableDuringTask is a pointer so a frontend entry omits it
	// entirely, like op/method/path: the field describes route behavior
	// and a frontend command has no route. A client reading an older
	// server that omits it for a CONTROL command must assume false.
	AvailableDuringTask *bool  `json:"available_during_task,omitempty"`
	Destructive         bool   `json:"destructive,omitempty"`
	Method              string `json:"method,omitempty"`
	Path                string `json:"path,omitempty"`
}

type serveSupportJSON struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// handleCommands returns the resolved registry. A frontend builds its
// slash-menu autocomplete from this response; a client that resolves
// commands itself, such as the Boxes console, also needs serve_support to
// know which entries it can run without a frontend in front of it.
func (s *Server) handleCommands(w http.ResponseWriter, _ *http.Request) {
	specs := command.NewRegistry().All()
	out := make([]commandEntryJSON, 0, len(specs))
	serveSupportOut := make(map[string]serveSupportJSON, len(specs))
	for _, spec := range specs {
		supported, reason := serveSupport(spec)
		serveSupportOut[spec.Name] = serveSupportJSON{Supported: supported, Reason: reason}
		e := commandEntryJSON{
			Name:        spec.Name,
			Aliases:     spec.Aliases,
			Kind:        string(spec.Kind),
			Op:          string(spec.Op),
			Summary:     spec.Summary,
			ArgHint:     spec.ArgHint,
			Category:    string(spec.Category),
			Destructive: spec.Destructive,
		}
		for _, a := range spec.Args {
			e.Args = append(e.Args, commandArgJSON{Name: a.Name, Type: string(a.Type), Optional: a.Optional})
		}
		if rt, ok := opRoutes[spec.Op]; ok {
			e.Method, e.Path = rt.method, rt.path
			avail := spec.AvailableDuringTask
			e.AvailableDuringTask = &avail
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, struct {
		Commands     []commandEntryJSON          `json:"commands"`
		ServeSupport map[string]serveSupportJSON `json:"serve_support"`
	}{Commands: out, ServeSupport: serveSupportOut})
}

// promptRoute selects which prompt-landing response shape
// resolvePromptCommand writes for a resolved command.
type promptRoute int

const (
	promptRouteAsync promptRoute = iota
	promptRouteEnqueue
	promptRouteSend
)

// commandReceiptJSON is the small "command" block every prompt-landing
// route's response carries when the request resolved to a command instead
// of an ordinary prompt.
type commandReceiptJSON struct {
	ID     string                `json:"id"`
	Status message.CommandStatus `json:"status"`
}

// resolvePromptCommand is the single entry point every prompt-landing
// route (prompt_async, enqueue, send) calls directly after
// parsePromptProvenance succeeds, before any other branch: it decides
// whether text is a TYPED slash command and, if so, resolves, records, and
// (for a dispatchable Op) runs it entirely in process — nothing reaches
// the model. See docs/design/slash-commands.md's "Serve-mode resolution"
// section for the status/text table this follows exactly.
//
// The typed check filters on the source the caller declares. Any holder of
// the run token can declare `typed`. The check only keeps an untagged
// caller's `/foo` text a prompt.
//
// reports whether text was a command and was handled (response already
// written). When handled is false, the caller sends promptText, which is
// res.Text for an escaped "//x" and text otherwise.
func (s *Server) resolvePromptCommand(w http.ResponseWriter, route promptRoute, id, text string,
	blobs []*message.Blob, prov engine.PromptProvenance, seq int64) (promptText string, handled bool) {
	if prov.Source.Normalized() != message.PromptSourceTyped {
		return text, false
	}

	res, err := command.NewRegistry().Resolve(text)
	if err != nil {
		if errors.Is(err, command.ErrNotCommand) {
			return res.Text, false
		}
		var unknown *command.UnknownCommandError
		if errors.As(err, &unknown) {
			return text, false
		}
		var argsErr *command.ArgsError
		if errors.As(err, &argsErr) {
			rec := message.CommandRecord{
				ID:          engine.NewCommandID(),
				Line:        text,
				Name:        argsErr.Spec.Name,
				Source:      prov.Source,
				SourceID:    prov.SourceID,
				SourceLabel: prov.SourceLabel,
				Status:      message.CommandFailed,
				Text:        err.Error(),
			}
			return "", s.writeCommand(w, route, id, seq, rec, nil)
		}
		// Resolve returns only the three error shapes handled above.
		return text, false
	}

	typed := typedCommandName(text)
	rec := message.CommandRecord{
		ID:          engine.NewCommandID(),
		Line:        text,
		Name:        res.Spec.Name,
		Args:        res.Args,
		Source:      prov.Source,
		SourceID:    prov.SourceID,
		SourceLabel: prov.SourceLabel,
	}
	if len(blobs) > 0 {
		rec.Status = message.CommandFailed
		rec.Text = fmt.Sprintf("/%s takes no attachments; nothing ran", typed)
		return "", s.writeCommand(w, route, id, seq, rec, nil)
	}
	if supported, _ := serveSupport(res.Spec); !supported {
		rec.Status = message.CommandUnsupported
		rec.Text = fmt.Sprintf("/%s is not available in this client", typed)
		return "", s.writeCommand(w, route, id, seq, rec, nil)
	}
	if !res.Spec.AvailableDuringTask && s.resolveLive(id).status() == "busy" {
		rec.Status = message.CommandRefused
		rec.Text = fmt.Sprintf("/%s cannot run while a turn is running; send it again after the turn ends", typed)
		return "", s.writeCommand(w, route, id, seq, rec, nil)
	}
	rec.Status = message.CommandAccepted
	return "", s.writeCommand(w, route, id, seq, rec, &res)
}

// typedCommandName extracts the name (or alias) the caller actually typed
// from a line Resolve just accepted — e.g. "clear" for a line whose
// canonical Resolution.Spec.Name is "new". Every status text uses this,
// never Spec.Name (see docs/design/slash-commands.md's "Status and text"
// table).
func typedCommandName(line string) string {
	body := strings.TrimPrefix(line, "/")
	if i := strings.IndexFunc(body, unicode.IsSpace); i >= 0 {
		return body[:i]
	}
	return body
}

// writeCommand resolves the mutable session, admits and records rec as the
// command's first durable record, writes the route's response, and — when
// res is non-nil (rec.Status is CommandAccepted) — starts the dispatch
// goroutine. Always returns true: every path through this function writes
// a response.
func (s *Server) writeCommand(w http.ResponseWriter, route promptRoute, id string, seq int64, rec message.CommandRecord, res *command.Resolution) bool {
	sess, releaseSess, ok := s.mutableSession(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return true
	}
	// handedOff is set only right before the dispatch goroutine takes over
	// the pin (and the admit slot). Until then, these defers own both, so a
	// panic anywhere below — a RecordCommand/RecordCommandDurable panic, or
	// one from writeJSON/writeErr — releases them instead of leaking them.
	handedOff := false
	defer func() {
		if !handedOff {
			releaseSess()
		}
	}()

	dispatching := res != nil
	if dispatching {
		if !s.admitCommand() {
			writeErr(w, http.StatusServiceUnavailable, "server shutting down")
			return true
		}
		defer func() {
			if !handedOff {
				s.wg.Done()
			}
		}()
	}

	if route == promptRouteEnqueue {
		dup, err := sess.RecordCommandDurable(rec, seq)
		if dup {
			writeJSON(w, http.StatusOK, enqueueResponse{Status: "duplicate", Watermark: sess.EnqueueSeq()})
			return true
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "command not durable: "+err.Error())
			return true
		}
	} else if err := sess.RecordCommand(rec); err != nil {
		writeErr(w, http.StatusInternalServerError, "command not recorded: "+err.Error())
		return true
	}

	receipt := &commandReceiptJSON{ID: rec.ID, Status: rec.Status}
	switch route {
	case promptRouteAsync:
		writeJSON(w, http.StatusAccepted, promptAsyncResponse{Seq: s.currentSeq(), Status: "command", Command: receipt})
	case promptRouteSend:
		writeJSON(w, http.StatusAccepted, map[string]any{"session_id": id, "status": "command", "command": receipt})
	case promptRouteEnqueue:
		writeJSON(w, http.StatusAccepted, enqueueResponse{Status: "command", Watermark: sess.EnqueueSeq(), Command: receipt})
	}
	if dispatching {
		// Ownership of the pin and the admit slot moves to runCommand here;
		// the defers above become no-ops.
		handedOff = true
		go s.runCommand(id, sess, releaseSess, rec, *res)
	}
	return true
}

// mutableSession resolves the one *engine.Session id's next durable
// mutation must land on: a managed CHILD comes straight from
// SessionManager's own resident node, never a second cold-loaded object
// over the same on-disk log; a root goes through the ordinary s.sessions
// residency map, cold-loading and racing exactly like claimForPrompt's own
// cold path (an insert race against a concurrent request, and eviction).
// Extracted from handleSetModel's identical selection block — see its own
// doc comment for why two *engine.Session for one log must never both be
// mutated. Used only by resolvePromptCommand and its own callees; every
// other handler keeps its own copy.
//
// For a root, it pins the resolved sessionState (sessionState.pins, under
// the same s.mu critical section that finds or inserts it) so
// evictResidentLocked cannot unload it before the caller calls release —
// otherwise a command's accepted and terminal records could land on two
// different *engine.Session for the same id (see writeCommand and
// runCommand). release decrements the pin and is safe to call more than
// once. A managed child has no residency entry to pin; its release is a
// no-op.
func (s *Server) mutableSession(id string) (sess *engine.Session, release func(), ok bool) {
	if child, ok := s.sessMgr.Session(id); ok && child.TaskParentID() != "" {
		return child, func() {}, true
	}
	s.mu.Lock()
	st := s.sessions[id]
	if st != nil {
		st.pins++
	}
	s.mu.Unlock()
	if st == nil {
		loaded, err := s.opts.LoadSession(id)
		if err != nil {
			return nil, nil, false
		}
		s.mu.Lock()
		var evicted []*engine.Session
		if ex := s.sessions[id]; ex != nil {
			st = ex // a resident appeared while we loaded; use the winner
		} else {
			st = &sessionState{sess: loaded, lastUsed: time.Now()}
			s.sessions[id] = st
		}
		// Pin before the sweep: otherwise a freshly inserted entry, still at
		// pins == 0, is its own sweep's eviction candidate whenever every
		// other resident is running or pinned.
		st.pins++
		if st.sess == loaded {
			evicted = s.evictResidentLocked()
		}
		s.mu.Unlock()
		releaseEvicted(evicted)
	}
	released := false
	release = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if released {
			return
		}
		released = true
		st.pins--
	}
	return st.sess, release, true
}

// admitCommand claims one Drain-visible slot for a dispatched command's
// background goroutine, the same admission claimForPrompt performs for an
// ordinary prompt turn: under s.mu, refuse once draining has started,
// otherwise wg.Add(1) before releasing the lock, so Drain's wg.Wait can
// never observe an Add that raced past draining=true.
func (s *Server) admitCommand() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.wg.Add(1)
	return true
}

// isDraining reports whether Drain has begun, for runCommand's terminal
// outcome mapping (a non-2xx route error during drain is "interrupted",
// not "failed").
func (s *Server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}
