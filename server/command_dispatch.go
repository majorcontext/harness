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
//
// Write caps the buffer at commandResultCap+1 bytes so a handler with an
// unbounded 2xx body — GET /session/{id}/queue on a long queue, for
// instance — never grows the buffer past that bound. It still reports the
// caller's full byte count with a nil error, matching io.Writer's contract,
// so a handler that checks its own Write result behaves exactly as it does
// against a real http.ResponseWriter.
//
// OpCompact is exempt from the cap: commandOutcome never journals its route
// body verbatim, only a small object it derives from the parsed
// compactResponseJSON (see compactCommandOutcome), so buffering the whole
// body here — including an oversized *message.Message summary — costs
// nothing durable and lets that derivation see every field it needs.
type commandResponseWriter struct {
	header http.Header
	op     command.Op
	code   int
	body   bytes.Buffer
}

func newCommandResponseWriter(op command.Op) *commandResponseWriter {
	return &commandResponseWriter{header: make(http.Header), op: op}
}

func (w *commandResponseWriter) Header() http.Header { return w.header }

func (w *commandResponseWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if w.op == command.OpCompact {
		w.body.Write(b)
		return len(b), nil
	}
	if room := commandResultCap + 1 - w.body.Len(); room > 0 {
		keep := b
		if len(keep) > room {
			keep = keep[:room]
		}
		w.body.Write(keep)
	}
	return len(b), nil
}

func (w *commandResponseWriter) WriteHeader(code int) { w.code = code }

// runCommand runs one dispatched command's route call and records its
// terminal outcome on sess, the same *engine.Session writeCommand recorded
// the accepted record on and pinned for this whole call. The caller holds
// the admitCommand slot and sess's pin; the deferred releaseSess and
// wg.Done release them, in that order (defers run LIFO), strictly after
// every terminal write below — including the panic path's — so Drain can
// never observe wg.Done before the pin it protected has lifted.
//
// net/http recovers a handler panic only on the request goroutine, so the
// deferred recover records a failed outcome instead of crashing the process.
func (s *Server) runCommand(id string, sess *engine.Session, releaseSess func(), rec message.CommandRecord, res command.Resolution) {
	defer s.wg.Done()
	defer releaseSess()

	typed := typedCommandName(rec.Line)
	defer func() {
		if r := recover(); r != nil {
			s.reportError(fmt.Errorf("command %s: handler panic: %v", rec.ID, r))
			rec.Status = message.CommandFailed
			rec.Text = fmt.Sprintf("/%s failed: internal error", typed)
			rec.Result = nil
			rec.ResultTruncated = false
			s.recordCommandTerminal(sess, rec)
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
		s.recordCommandTerminal(sess, rec)
		return
	}
	req.SetPathValue("id", id)

	if s.commandDispatchRace != nil {
		s.commandDispatchRace() // test-only seam, see its own doc comment
	}

	cw := newCommandResponseWriter(res.Spec.Op)
	serveOpHandlers[res.Spec.Op](s, cw, req)

	managedChild := sess.TaskParentID() != ""
	rec.Status, rec.Text, rec.Result, rec.ResultTruncated = commandOutcome(res.Spec.Op, typed, cw.code, cw.body.Bytes(), s.isDraining(), res.Spec.AvailableDuringTask, managedChild)
	s.recordCommandTerminal(sess, rec)
}

// recordCommandTerminal writes rec's terminal status to sess, the pinned
// object runCommand's caller resolved for id — no re-resolve: the pin held
// across the whole dispatch guarantees sess is still the one resident
// object for id, so a fresh mutableSession lookup would only ever return
// the same object.
func (s *Server) recordCommandTerminal(sess *engine.Session, rec message.CommandRecord) {
	if err := sess.RecordCommand(rec); err != nil {
		s.reportError(fmt.Errorf("command %s: record terminal status: %w", rec.ID, err))
	}
}

// commandResultCap bounds a dispatched command's Result field — the
// route's own 2xx JSON body — at 16 KiB (see message.CommandRecord's own
// doc comment). Over the cap, Result is omitted and ResultTruncated is true
// instead of journaling an unbounded response body. OpCompact does not use
// this cap; see compactCommandOutcome.
const commandResultCap = 16 << 10

// commandOutcome maps one route call's HTTP result to a terminal
// CommandRecord status/text/result, per docs/design/slash-commands.md's
// "Status and text" table: a 2xx with a non-empty compact skip_reason is
// "failed" with the skip sentence; any other 2xx is "succeeded"; a non-2xx
// while draining is "interrupted" with the drain wording; a 409 for an Op
// whose spec is not availableDuringTask, on a session that is not a
// managed child, is "refused" with the same sentence resolvePromptCommand's
// own pre-dispatch busy check uses — a turn that started in the gap
// between that check and this dispatch blocks the op exactly like one
// already running at check time, from either 409 cause (this session's
// own turn, or another session's turn holding the workdir); any other
// non-2xx is "failed" with the route's own error text — including a 409
// on a managed child, which is rejectManagedChildTurn's routing rejection,
// never a busy-turn conflict.
func commandOutcome(op command.Op, typed string, code int, body []byte, draining bool, availableDuringTask bool, managedChild bool) (status message.CommandStatus, text string, result json.RawMessage, truncated bool) {
	if code >= 200 && code < 300 {
		if op == command.OpCompact {
			return compactCommandOutcome(typed, body)
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
	if code == http.StatusConflict && !availableDuringTask && !managedChild {
		return message.CommandRefused, fmt.Sprintf("/%s cannot run while a turn is running; send it again after the turn ends", typed), nil, false
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

// compactResultJSON is OpCompact's own CommandRecord.Result shape — never
// the route's full compactResponseJSON body, which duplicates the fold's
// summary message already durable in history under SummaryID. Built from
// the fully buffered (uncapped, see commandResponseWriter) route response,
// so it never depends on commandResultCap and never truncates.
type compactResultJSON struct {
	TurnsFolded int    `json:"turns_folded"`
	FirstID     string `json:"first_id"`
	LastID      string `json:"last_id"`
	SummaryID   string `json:"summary_id"`
}

// compactDelegatedResultJSON is OpCompact's Result shape on the Claude Code
// delegated lane, where no native fold happened at all.
type compactDelegatedResultJSON struct {
	ClaudeCodeDelegated bool `json:"claude_code_delegated"`
}

// compactCommandOutcome maps a 2xx POST /session/{id}/compact body to a
// terminal CommandRecord for OpCompact. See compactResultJSON's own doc
// comment for why Result is this slim, derived object rather than the
// route's own compactResponseJSON body.
func compactCommandOutcome(typed string, body []byte) (message.CommandStatus, string, json.RawMessage, bool) {
	var cr compactResponseJSON
	_ = json.Unmarshal(body, &cr)
	if cr.SkipReason != "" {
		return message.CommandFailed, "/compact did nothing: " + engine.CompactSkipMessage(cr.SkipReason), nil, false
	}
	text := fmt.Sprintf("/%s succeeded", typed)
	var slim any
	if cr.ClaudeCodeDelegated {
		slim = compactDelegatedResultJSON{ClaudeCodeDelegated: true}
	} else {
		summaryID := ""
		if cr.Summary != nil {
			summaryID = cr.Summary.ID
		}
		slim = compactResultJSON{
			TurnsFolded: cr.TurnsFolded,
			FirstID:     cr.FirstID,
			LastID:      cr.LastID,
			SummaryID:   summaryID,
		}
	}
	result, err := json.Marshal(slim)
	if err != nil {
		// Unreachable: both slim shapes above are plain structs of strings
		// and a bool.
		return message.CommandSucceeded, text, nil, false
	}
	return message.CommandSucceeded, text, result, false
}
