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

func commandRouteBody(op command.Op, args map[string]any) []byte {
	if len(args) == 0 {
		if op == command.OpCompact {
			return []byte("{}")
		}
		return nil
	}
	body, _ := json.Marshal(args)
	return body
}

// commandResponseWriter caps its buffer at commandResultCap+1 bytes.
// OpCompact is exempt: it never journals the body verbatim (see
// compactCommandOutcome).
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

// net/http recovers a handler panic only on the request goroutine, so the
// deferred recover records a failed outcome instead of crashing the process.
func (s *Server) runCommand(id string, sess *engine.Session, releaseSess func(), rec message.CommandRecord, res command.Resolution) {
	defer s.wg.Done()
	// Release the session pin before wg.Done so Drain cannot return with the pin held.
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
		// Unreachable in practice; fail closed rather than leave the command
		// stuck "accepted" forever.
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
	rt.handler(s, cw, req)

	managedChild := sess.TaskParentID() != ""
	rec.Status, rec.Text, rec.Result, rec.ResultTruncated = commandOutcome(res.Spec.Op, typed, cw.code, cw.body.Bytes(), s.isDraining(), res.Spec.AvailableDuringTask, managedChild)
	s.recordCommandTerminal(sess, rec)
}

func (s *Server) recordCommandTerminal(sess *engine.Session, rec message.CommandRecord) {
	if err := sess.RecordCommand(rec); err != nil {
		s.reportError(fmt.Errorf("command %s: record terminal status: %w", rec.ID, err))
	}
}

// commandResultCap bounds a dispatched command's Result field. OpCompact
// does not use it; see compactCommandOutcome.
const commandResultCap = 16 << 10

// A 409 for an Op not availableDuringTask, on a non-managed-child session,
// is "refused" rather than "failed".
func commandOutcome(op command.Op, typed string, code int, body []byte, draining bool, availableDuringTask bool, managedChild bool) (status message.CommandStatus, text string, result json.RawMessage, truncated bool) {
	if code >= 200 && code < 300 {
		if op == command.OpCompact {
			return compactCommandOutcome(typed, body)
		}
		text = fmt.Sprintf("/%s succeeded", typed)
		if len(body) > commandResultCap {
			return message.CommandSucceeded, text, nil, true
		}
		// A handler that never calls writeJSON could hand back a non-JSON
		// 2xx body, which would fail writeRecord's marshal if embedded verbatim.
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

// compactResultJSON is OpCompact's own Result shape — never the route's
// full compactResponseJSON body, which duplicates history.
type compactResultJSON struct {
	TurnsFolded int    `json:"turns_folded"`
	FirstID     string `json:"first_id"`
	LastID      string `json:"last_id"`
	SummaryID   string `json:"summary_id"`
}

type compactDelegatedResultJSON struct {
	ClaudeCodeDelegated bool `json:"claude_code_delegated"`
}

type compactCommandResponseJSON struct {
	TurnsFolded         int    `json:"turns_folded"`
	FirstID             string `json:"first_id"`
	LastID              string `json:"last_id"`
	SkipReason          string `json:"skip_reason"`
	ClaudeCodeDelegated bool   `json:"claude_code_delegated"`
	Summary             *struct {
		ID string `json:"id"`
	} `json:"summary"`
}

func compactCommandOutcome(typed string, body []byte) (message.CommandStatus, string, json.RawMessage, bool) {
	var cr compactCommandResponseJSON
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
