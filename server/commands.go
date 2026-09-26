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

// serveModeOps declares which control Ops serve mode resolves through its
// own routes, total over every command.Op. queue-clear stays false: nothing
// dispatches it through serve mode yet.
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

const serveUnsupportedReason = "Not available in this client."

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

type promptRoute int

const (
	promptRouteAsync promptRoute = iota
	promptRouteEnqueue
	promptRouteSend
)

type commandReceiptJSON struct {
	ID        string                `json:"id"`
	Status    message.CommandStatus `json:"status"`
	ClientRef string                `json:"client_ref,omitempty"`
}

// resolvePromptCommand decides whether text is a typed slash command and,
// if so, resolves, records, and dispatches it entirely in process. Reports
// whether it was handled (response already written).
func (s *Server) resolvePromptCommand(w http.ResponseWriter, route promptRoute, id, text string,
	blobs []*message.Blob, prov engine.PromptProvenance, seq int64, clientRef string) (promptText string, handled bool) {
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
				ClientRef:   clientRef,
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
		ClientRef:   clientRef,
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

func typedCommandName(line string) string {
	body := strings.TrimPrefix(line, "/")
	if i := strings.IndexFunc(body, unicode.IsSpace); i >= 0 {
		return body[:i]
	}
	return body
}

func (s *Server) writeCommand(w http.ResponseWriter, route promptRoute, id string, seq int64, rec message.CommandRecord, res *command.Resolution) bool {
	sess, releaseSess, ok := s.mutableSession(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return true
	}
	// Until handedOff, these defers own the pin and admit slot, so a panic
	// anywhere below releases them instead of leaking them.
	handedOff := false
	admitted := false
	defer func() {
		if !handedOff {
			releaseSess()
			if admitted {
				s.wg.Done()
			}
		}
	}()

	if !s.admitCommand() {
		writeErr(w, http.StatusServiceUnavailable, "server shutting down")
		return true
	}
	admitted = true

	dispatching := res != nil

	// Sampled before the durable record is written, so a resuming
	// GET /event?from=fromSeq caller still replays the accepted event.
	fromSeq := s.currentSeq()

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

	receipt := &commandReceiptJSON{ID: rec.ID, Status: rec.Status, ClientRef: rec.ClientRef}
	switch route {
	case promptRouteAsync:
		writeJSON(w, http.StatusAccepted, promptAsyncResponse{Seq: fromSeq, Status: "command", Command: receipt})
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
// mutation must land on. For a root, it pins the resolved sessionState so
// evictResidentLocked cannot unload it before release. release is safe to
// call more than once; a managed child's release is a no-op.
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
			evicted = append(evicted, loaded)
		} else {
			st = &sessionState{sess: loaded, lastUsed: time.Now()}
			s.sessions[id] = st
		}
		// Pin before the sweep, or a freshly inserted entry at pins == 0
		// becomes its own sweep's eviction candidate.
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

func (s *Server) admitCommand() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.wg.Add(1)
	return true
}

func (s *Server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}
