package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

type commandResponseBody struct {
	Status  string `json:"status"`
	Command struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"command"`
}

func decodeCommandResponse(t *testing.T, data []byte) commandResponseBody {
	t.Helper()
	var body commandResponseBody
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode command response: %v (%s)", err, data)
	}
	return body
}

func (h *harness) sessionDirect(id string) *engine.Session {
	h.t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	st := h.srv.sessions[id]
	if st == nil {
		h.t.Fatalf("session %s is not resident", id)
	}
	return st.sess
}

func TestTypedCompactNeverAppendsUserMessage(t *testing.T) {
	prov := newCapturingProvider()
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/compact"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	body := decodeCommandResponse(t, data)
	if body.Status != "command" || body.Command.Status != "accepted" {
		t.Fatalf("response = %+v, want status=command command.status=accepted", body)
	}

	accepted := sse.waitFor(t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("first command event = %+v, want accepted", accepted.Command)
	}
	terminal := sse.waitFor(t, "command")
	if terminal.Command == nil {
		t.Fatal("terminal command event carries no Command")
	}
	if terminal.Command.Status != message.CommandFailed {
		t.Fatalf("terminal command status = %q, want failed", terminal.Command.Status)
	}
	want := "/compact did nothing: " + engine.CompactSkipMessage(engine.SkipReasonNotEnoughTurns)
	if terminal.Command.Text != want {
		t.Errorf("failed text = %q, want %q", terminal.Command.Text, want)
	}

	if len(prov.requests) != 0 {
		t.Errorf("provider received %d requests, want 0 (the model must never see /compact)", len(prov.requests))
	}
	for _, m := range h.userMessages(id) {
		if m.Parts.Text() == "/compact" {
			t.Fatalf("a user message holds /compact: %+v", m)
		}
	}
}

func TestSlashLineStaysOrdinaryPrompt(t *testing.T) {
	cases := []struct {
		name     string
		body     map[string]any
		wantText string
	}{
		{"empty source", map[string]any{"parts": []map[string]string{{"type": "text", "text": "/model x/y"}}}, "/model x/y"},
		{"api source", map[string]any{"parts": []map[string]string{{"type": "text", "text": "/model x/y"}}, "source": "api"}, "/model x/y"},
		{"schedule source", map[string]any{"parts": []map[string]string{{"type": "text", "text": "/model x/y"}}, "source": "schedule"}, "/model x/y"},
		{"cross_box source", map[string]any{"parts": []map[string]string{{"type": "text", "text": "/model x/y"}}, "source": "cross_box"}, "/model x/y"},
		{"empty source /compact", map[string]any{"parts": []map[string]string{{"type": "text", "text": "/compact"}}}, "/compact"},
		{"api source /compact", map[string]any{"parts": []map[string]string{{"type": "text", "text": "/compact"}}, "source": "api"}, "/compact"},
		{"typed escaped //compact", map[string]any{"parts": []map[string]string{{"type": "text", "text": "//compact"}}, "source": "typed"}, "/compact"},
		{"typed escaped //model", map[string]any{"parts": []map[string]string{{"type": "text", "text": "//model x"}}, "source": "typed"}, "/model x"},
		{"non-typed //model stays literal", map[string]any{"parts": []map[string]string{{"type": "text", "text": "//model x"}}}, "//model x"},
		{"unknown typed command", map[string]any{"parts": []map[string]string{{"type": "text", "text": "/cost"}}, "source": "typed"}, "/cost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := newCapturingProvider(asstTurn("ok"))
			h := newHarness(t, prov)
			id := h.createSession("test/m1")

			resp, data := h.do("POST", "/session/"+id+"/prompt_async", tc.body)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
			}
			h.waitIdle(id)

			users := h.userMessages(id)
			if len(users) != 1 || users[0].Parts.Text() != tc.wantText {
				t.Fatalf("user messages = %+v, want one with text %q", users, tc.wantText)
			}
			if cmds := h.sessionDirect(id).Commands(); len(cmds) != 0 {
				t.Errorf("Commands() = %+v, want none", cmds)
			}
		})
	}
}

func TestTypedCommandOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		parts      []any
		async      bool
		wantStatus string
		wantText   string
		wantName   string
	}{
		{
			name:       "bad args",
			parts:      []any{map[string]string{"type": "text", "text": "/compact abc"}},
			wantStatus: "failed",
			wantText:   `command: /compact keep_turns must be a number, got "abc"`,
			wantName:   "compact",
		},
		{
			name: "attachment",
			parts: []any{
				map[string]string{"type": "text", "text": "/model a/b"},
				attachmentPart("image/png", testPNG(t)),
			},
			wantStatus: "failed",
			wantText:   "/model takes no attachments; nothing ran",
		},
		{
			name:       "route error",
			parts:      []any{map[string]string{"type": "text", "text": "/model nope/x"}},
			async:      true,
			wantStatus: "failed",
			wantText:   `provider "nope" is not configured`,
		},
		{
			name:       "/new unsupported",
			parts:      []any{map[string]string{"type": "text", "text": "/new"}},
			wantStatus: "unsupported",
			wantText:   "/new is not available in this client",
		},
		{
			name:       "/clear unsupported",
			parts:      []any{map[string]string{"type": "text", "text": "/clear"}},
			wantStatus: "unsupported",
			wantText:   "/clear is not available in this client",
		},
		{
			name:       "/queue-clear unsupported",
			parts:      []any{map[string]string{"type": "text", "text": "/queue-clear"}},
			wantStatus: "unsupported",
			wantText:   "/queue-clear is not available in this client",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := newCapturingProvider()
			h := newHarness(t, prov)
			id := h.createSession("test/m1")
			var sse *sseStream
			if tc.async {
				sse = h.openSSE("?from=0", "")
			}

			resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
				"parts": tc.parts, "source": "typed",
			})
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
			}

			if tc.async {
				accepted := sse.waitFor(t, "command")
				if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
					t.Fatalf("first command event = %+v, want accepted", accepted.Command)
				}
				terminal := sse.waitFor(t, "command")
				if terminal.Command == nil || string(terminal.Command.Status) != tc.wantStatus {
					t.Fatalf("terminal command event = %+v, want status=%s", terminal.Command, tc.wantStatus)
				}
				if terminal.Command.Text != tc.wantText {
					t.Errorf("text = %q, want %q", terminal.Command.Text, tc.wantText)
				}
				return
			}

			body := decodeCommandResponse(t, data)
			if body.Command.Status != tc.wantStatus {
				t.Fatalf("command.status = %q, want %q", body.Command.Status, tc.wantStatus)
			}
			cmds := h.sessionDirect(id).Commands()
			if len(cmds) != 1 {
				t.Fatalf("Commands() = %+v, want 1", cmds)
			}
			if cmds[0].Text != tc.wantText {
				t.Errorf("text = %q, want %q", cmds[0].Text, tc.wantText)
			}
			if tc.wantName != "" && cmds[0].Name != tc.wantName {
				t.Errorf("name = %q, want %q", cmds[0].Name, tc.wantName)
			}
		})
	}
}

func TestMidTurnCompactRefusedNotQueued(t *testing.T) {
	prov := &queueProv{name: "test", started: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	defer close(prov.release)
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "occupant"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("occupant prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/compact"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/compact status %d: %s", resp.StatusCode, data)
	}
	body := decodeCommandResponse(t, data)
	if body.Command.Status != "refused" {
		t.Fatalf("command.status = %q, want refused", body.Command.Status)
	}
	if q := h.sessionDirect(id).QueuedPrompts(); len(q) != 0 {
		t.Fatalf("QueuedPrompts() = %+v, want empty", q)
	}

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/model test/m2"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/model status %d: %s", resp.StatusCode, data)
	}
	body = decodeCommandResponse(t, data)
	if body.Command.Status != "accepted" {
		t.Fatalf("/model command.status = %q, want accepted", body.Command.Status)
	}

	refused := sse.waitFor(t, "command")
	if refused.Command == nil || refused.Command.Status != message.CommandRefused {
		t.Fatalf("first command event = %+v, want refused", refused.Command)
	}
	accepted := sse.waitFor(t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("second command event = %+v, want accepted", accepted.Command)
	}
	terminal := sse.waitFor(t, "command")
	if terminal.Command == nil || terminal.Command.Status != message.CommandSucceeded {
		t.Fatalf("third command event = %+v, want succeeded", terminal.Command)
	}
}

func TestEnqueueCommandSeqRunsOnce(t *testing.T) {
	prov := newCapturingProvider(asstTurn("ok"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	body := map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/thinking high"}},
		"seq":    int64(4),
		"source": "typed",
	}
	resp, data := h.do("POST", "/session/"+id+"/enqueue", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first enqueue status %d: %s", resp.StatusCode, data)
	}
	first := decodeCommandResponse(t, data)
	if first.Status != "command" || first.Command.Status != "accepted" {
		t.Fatalf("first response = %+v, want status=command command.status=accepted", first)
	}

	accepted := sse.waitFor(t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("first command event = %+v, want accepted", accepted.Command)
	}
	succeeded := sse.waitFor(t, "command")
	if succeeded.Command == nil || succeeded.Command.Status != message.CommandSucceeded {
		t.Fatalf("second command event = %+v, want succeeded", succeeded.Command)
	}

	resp, data = h.do("POST", "/session/"+id+"/enqueue", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second enqueue status %d: %s", resp.StatusCode, data)
	}
	var second struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &second); err != nil {
		t.Fatal(err)
	}
	if second.Status != "duplicate" {
		t.Fatalf("second enqueue status = %q, want duplicate", second.Status)
	}

	if cmds := commandEventsForSession(h.srv, id); len(cmds) != 2 {
		t.Fatalf("command events = %d, want 2 (accepted, succeeded): %+v", len(cmds), cmds)
	}
}

func TestCommandOutcomeMapping(t *testing.T) {
	bigBody := bytes.Repeat([]byte("a"), 17<<10)
	cases := []struct {
		name                string
		op                  command.Op
		opName              string
		code                int
		body                []byte
		draining            bool
		availableDuringTask bool
		managedChild        bool
		wantStatus          message.CommandStatus
		wantText            string
		wantTruncated       bool
	}{
		{
			name: "over cap truncates and drops result", op: command.OpStatus, opName: "status",
			code: http.StatusOK, body: bigBody, availableDuringTask: true,
			wantStatus: message.CommandSucceeded, wantTruncated: true,
		},
		{
			name: "invalid json omits result", op: command.OpStatus, opName: "status",
			code: http.StatusOK, body: []byte("not json"), availableDuringTask: true,
			wantStatus: message.CommandSucceeded, wantText: "/status succeeded",
		},
		{
			name: "draining non-2xx is interrupted", op: command.OpStatus, opName: "status",
			code: http.StatusInternalServerError, body: []byte(`{"error":"boom"}`), draining: true, availableDuringTask: true,
			wantStatus: message.CommandInterrupted,
			wantText:   "harness stopped before /status finished; it will not run again",
		},
		{
			name: "409 not available during task is refused", op: command.OpCompact, opName: "compact",
			code: http.StatusConflict, body: []byte(`{"error":"session is busy with another prompt"}`),
			wantStatus: message.CommandRefused,
			wantText:   "/compact cannot run while a turn is running; send it again after the turn ends",
		},
		{
			name: "409 available during task keeps failed", op: command.OpCompact, opName: "compact",
			code: http.StatusConflict, body: []byte(`{"error":"session is busy with another prompt"}`), availableDuringTask: true,
			wantStatus: message.CommandFailed, wantText: "session is busy with another prompt",
		},
		{
			name: "409 managed child keeps failed", op: command.OpCompact, opName: "compact",
			code: http.StatusConflict, body: []byte(`{"error":"session is a SessionManager-managed child session; use POST /session/{id}/send instead"}`), managedChild: true,
			wantStatus: message.CommandFailed,
			wantText:   "session is a SessionManager-managed child session; use POST /session/{id}/send instead",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, text, result, truncated := commandOutcome(tc.op, tc.opName, tc.code, tc.body, tc.draining, tc.availableDuringTask, tc.managedChild)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if tc.wantText != "" && text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if result != nil {
				t.Errorf("result = %q, want nil", result)
			}
			if truncated != tc.wantTruncated {
				t.Errorf("truncated = %v, want %v", truncated, tc.wantTruncated)
			}
		})
	}
}

func TestCommandResponseWriterCapsBufferedBody(t *testing.T) {
	cw := newCommandResponseWriter(command.OpQueueList)
	chunk := bytes.Repeat([]byte("a"), 4096)
	total := 1 << 20
	written := 0
	for written < total {
		n, err := cw.Write(chunk)
		if err != nil {
			t.Fatalf("Write returned error %v, want nil", err)
		}
		if n != len(chunk) {
			t.Fatalf("Write returned n = %d, want %d (len(p))", n, len(chunk))
		}
		written += n
	}
	if cw.body.Len() > commandResultCap+1 {
		t.Fatalf("buffered body = %d bytes, want at most %d (commandResultCap+1)", cw.body.Len(), commandResultCap+1)
	}

	status, _, result, truncated := commandOutcome(command.OpQueueList, "queue", cw.code, cw.body.Bytes(), false, true, false)
	if status != message.CommandSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if !truncated {
		t.Error("truncated = false, want true")
	}
	if result != nil {
		t.Errorf("result = %q, want nil", result)
	}
}

func TestTypedCommandRefusedWhileDraining(t *testing.T) {
	cases := []struct {
		name  string
		parts []any
	}{
		{
			name:  "bad args",
			parts: []any{map[string]string{"type": "text", "text": "/compact abc"}},
		},
		{
			name: "attachment",
			parts: []any{
				map[string]string{"type": "text", "text": "/model a/b"},
				attachmentPart("image/png", testPNG(t)),
			},
		},
		{
			name:  "unsupported",
			parts: []any{map[string]string{"type": "text", "text": "/new"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newCapturingProvider())
			id := h.createSession("test/m1")
			h.srv.mu.Lock()
			h.srv.draining = true
			h.srv.mu.Unlock()

			resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
				"parts": tc.parts, "source": "typed",
			})
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("prompt_async status %d, want 503: %s", resp.StatusCode, data)
			}
			if cmds := h.sessionDirect(id).Commands(); len(cmds) != 0 {
				t.Fatalf("Commands() = %+v, want none", cmds)
			}
			if events := commandEventsForSession(h.srv, id); len(events) != 0 {
				t.Fatalf("command events = %+v, want none", events)
			}
		})
	}
}

func TestMidTurnCommandRefusedWhileDraining(t *testing.T) {
	prov := &queueProv{name: "test", started: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	defer close(prov.release)

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "occupant"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("occupant prompt status %d, want 202: %s", resp.StatusCode, data)
	}
	<-prov.started
	h.srv.mu.Lock()
	h.srv.draining = true
	h.srv.mu.Unlock()

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "/compact"}}, "source": "typed",
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/compact status %d, want 503: %s", resp.StatusCode, data)
	}
	if cmds := h.sessionDirect(id).Commands(); len(cmds) != 0 {
		t.Fatalf("Commands() = %+v, want none", cmds)
	}
	if events := commandEventsForSession(h.srv, id); len(events) != 0 {
		t.Fatalf("command events = %+v, want none", events)
	}
}

func TestDrainingEnqueueBadArgsDoesNotConsumeSequence(t *testing.T) {
	h := newHarness(t, newCapturingProvider())
	id := h.createSession("test/m1")
	body := map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/compact abc"}},
		"source": "typed",
		"seq":    17,
	}
	h.srv.mu.Lock()
	h.srv.draining = true
	h.srv.mu.Unlock()

	resp, data := h.do("POST", "/session/"+id+"/enqueue", body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("enqueue status %d, want 503: %s", resp.StatusCode, data)
	}
	if cmds := h.sessionDirect(id).Commands(); len(cmds) != 0 {
		t.Fatalf("Commands() = %+v, want none", cmds)
	}
	if events := commandEventsForSession(h.srv, id); len(events) != 0 {
		t.Fatalf("command events = %+v, want none", events)
	}

	h.srv.mu.Lock()
	h.srv.draining = false
	h.srv.mu.Unlock()
	resp, data = h.do("POST", "/session/"+id+"/enqueue", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue retry status %d, want 202: %s", resp.StatusCode, data)
	}
	if got := h.sessionDirect(id).EnqueueSeq(); got != 17 {
		t.Fatalf("enqueue watermark = %d, want 17", got)
	}
}
