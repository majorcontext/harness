package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// serveOpHandlers maps a dispatchable Op to the exact handler method its
// own route already uses — the in-process call IS that route: the prompt
// route that resolved this command already authenticated the caller, so
// this bypasses auth deliberately and reuses every other guard the handler
// itself performs (rejectManagedChildTurn, claimForPrompt,
// ModelSupported/CheckModel, ...). Total over every Op serveModeOps marks
// true — see TestServeOpHandlersCoverSupportedOps.
var serveOpHandlers = map[command.Op]func(*Server, http.ResponseWriter, *http.Request){
	command.OpCompact:        (*Server).handleCompact,
	command.OpSetModel:       (*Server).handleSetModel,
	command.OpSetThinking:    (*Server).handleSetThinking,
	command.OpSetServiceTier: (*Server).handleSetServiceTier,
	command.OpAbort:          (*Server).handleAbort,
	command.OpSetGoal:        (*Server).handleGoal,
	command.OpClearGoal:      (*Server).handleGoalDelete,
	command.OpQueueList:      (*Server).handleQueueGet,
	command.OpStatus:         (*Server).handleGet,
	command.OpProcessList:    (*Server).handleProcessList,
}

// commandRouteBody builds the JSON body serveOpHandlers[op] expects, from a
// resolved command's own Resolution.Args. Every Op not listed takes no
// body (nil): abort, clear_goal, queue_list, status, and process_list are
// all argument-free routes.
func commandRouteBody(op command.Op, args map[string]any) []byte {
	switch op {
	case command.OpCompact:
		if n, ok := args["keep_turns"].(int); ok {
			b, _ := json.Marshal(map[string]int{"keep_turns": n})
			return b
		}
		return []byte("{}")
	case command.OpSetModel:
		b, _ := json.Marshal(map[string]string{"model": args["model"].(string)})
		return b
	case command.OpSetThinking:
		b, _ := json.Marshal(map[string]string{"effort": args["effort"].(string)})
		return b
	case command.OpSetServiceTier:
		b, _ := json.Marshal(map[string]string{"service_tier": args["service_tier"].(string)})
		return b
	case command.OpSetGoal:
		b, _ := json.Marshal(map[string]string{"condition": args["condition"].(string)})
		return b
	default:
		return nil
	}
}

// commandResponseWriter is the minimal http.ResponseWriter runCommand
// hands serveOpHandlers[op]: a header map, the written status code
// (defaulting to 200, matching net/http's own WriteHeader contract, on a
// handler that never calls WriteHeader explicitly), and a buffered body.
type commandResponseWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newCommandResponseWriter() *commandResponseWriter {
	return &commandResponseWriter{header: make(http.Header)}
}

func (w *commandResponseWriter) Header() http.Header { return w.header }

func (w *commandResponseWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.body.Write(b)
}

func (w *commandResponseWriter) WriteHeader(code int) { w.code = code }

// runCommand performs one dispatched command's in-process route call and
// records its terminal outcome. resolvePromptCommand starts this in its
// own goroutine, already holding the admitCommand slot this function's
// deferred wg.Done releases.
//
// The accepted record's own *engine.Session is NOT passed in for the
// terminal write. serveOpHandlers[op] performs its own independent session
// lookup/cold-load, and residency eviction can happen in the gap between
// the two (that object is not running, so it is an ordinary LRU eviction
// candidate — see mutableSession and evictResidentLocked). Writing the
// terminal record through a stale reference would split one on-disk log
// across two live *engine.Session objects, the exact hazard
// handleSetModel's own doc comment names. recordCommandTerminal re-resolves
// fresh instead.
//
// A deferred recover guards serveOpHandlers[op]: net/http recovers a
// per-request handler panic itself, but this call runs off the request
// goroutine, so an unrecovered panic here would crash the process instead
// of failing one command. Recovering writes a failed terminal record rather
// than leaving the command "accepted" forever, and never re-panics.
func (s *Server) runCommand(id string, rec message.CommandRecord, res command.Resolution) {
	defer s.wg.Done()

	typed := typedCommandName(rec.Line)
	defer func() {
		if r := recover(); r != nil {
			s.reportError(fmt.Errorf("command %s: handler panic: %v", rec.ID, r))
			rec.Status = message.CommandFailed
			rec.Text = fmt.Sprintf("/%s failed: internal error", typed)
			rec.Result = nil
			rec.ResultTruncated = false
			s.recordCommandTerminal(id, rec)
		}
	}()

	rt := opRoutes[res.Spec.Op]
	body := commandRouteBody(res.Spec.Op, res.Args)
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), rt.method, strings.ReplaceAll(rt.path, "{id}", id), bodyReader)
	if err != nil {
		// Unreachable in practice: rt.method/rt.path are compile-time
		// constants from opRoutes. Fail closed rather than leave the
		// command stuck "accepted" forever.
		s.reportError(fmt.Errorf("command %s: build request: %w", rec.ID, err))
		rec.Status = message.CommandFailed
		rec.Text = err.Error()
		s.recordCommandTerminal(id, rec)
		return
	}
	req.SetPathValue("id", id)

	if s.commandDispatchRace != nil {
		s.commandDispatchRace() // test-only seam, see its own doc comment
	}

	cw := newCommandResponseWriter()
	serveOpHandlers[res.Spec.Op](s, cw, req)

	rec.Status, rec.Text, rec.Result, rec.ResultTruncated = commandOutcome(res.Spec.Op, typed, cw.code, cw.body.Bytes(), s.isDraining())
	s.recordCommandTerminal(id, rec)
}

// recordCommandTerminal writes rec's terminal status through id's CURRENT
// *engine.Session, resolved fresh via mutableSession rather than the
// object runCommand's own caller looked up before the route handler ran
// (see runCommand's doc comment for the eviction gap this closes). A
// session no longer resolvable (evicted and then removed, or a load
// failure) reports through reportError and writes nothing — never mutate
// a stale object to force the write through.
func (s *Server) recordCommandTerminal(id string, rec message.CommandRecord) {
	sess, ok := s.mutableSession(id)
	if !ok {
		s.reportError(fmt.Errorf("command %s: session %s is no longer resolvable for its terminal record", rec.ID, id))
		return
	}
	if err := sess.RecordCommand(rec); err != nil {
		s.reportError(fmt.Errorf("command %s: record terminal status: %w", rec.ID, err))
	}
}

// commandResultCap bounds a dispatched command's Result field — the
// route's own 2xx JSON body — at 16 KiB (see message.CommandRecord's own
// doc comment). Over the cap, Result is omitted and ResultTruncated is true
// instead of journaling an unbounded response body.
const commandResultCap = 16 << 10

// commandOutcome maps one route call's HTTP result to a terminal
// CommandRecord status/text/result, per docs/design/slash-commands.md's
// "Status and text" table: a 2xx with a non-empty compact skip_reason is
// "failed" with the skip sentence; any other 2xx is "succeeded"; a non-2xx
// while draining is "interrupted" with the drain wording; any other non-2xx
// is "failed" with the route's own error text.
func commandOutcome(op command.Op, typed string, code int, body []byte, draining bool) (status message.CommandStatus, text string, result json.RawMessage, truncated bool) {
	if code >= 200 && code < 300 {
		if op == command.OpCompact {
			var cr struct {
				SkipReason string `json:"skip_reason"`
			}
			_ = json.Unmarshal(body, &cr)
			if cr.SkipReason != "" {
				return message.CommandFailed, "/compact did nothing: " + engine.CompactSkipMessage(cr.SkipReason), nil, false
			}
		}
		text = fmt.Sprintf("/%s succeeded", typed)
		if len(body) > commandResultCap {
			return message.CommandSucceeded, text, nil, true
		}
		// A handler that never calls writeJSON (none does today) could hand
		// back a non-JSON 2xx body; embedding it verbatim as Result would
		// fail writeRecord's own marshal (json.RawMessage validates on
		// encode) and strand the command "accepted" until the next boot.
		if len(body) > 0 && json.Valid(body) {
			result = json.RawMessage(body)
		}
		return message.CommandSucceeded, text, result, false
	}
	if draining {
		return message.CommandInterrupted, fmt.Sprintf("harness stopped before /%s finished; it will not run again", typed), nil, false
	}
	var eb struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &eb)
	text = eb.Error
	if text == "" {
		text = http.StatusText(code)
	}
	return message.CommandFailed, text, nil, false
}
