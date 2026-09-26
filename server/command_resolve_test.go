package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// commandResponseBody is the "command" shape every prompt-landing route
// carries when a request resolved to a command (see resolvePromptCommand).
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

// sessionDirect returns id's own resident *engine.Session, the same way
// command_journal_test.go's tests reach into the harness for one.
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

// TestTypedCompactNeverAppendsUserMessage is the named-failure test for
// the feature's core promise: a typed "/compact" resolves entirely in
// process. The model never sees it, and it never becomes a user message —
// a regression that let it fall through to Session.Prompt would append a
// user message reading "/compact" and, on a session with turns to fold,
// bill and journal a normal turn instead of a command record.
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
	// A fresh session has no turns to fold, so /compact always skips — never
	// succeeds. Asserting the exact failed text (rather than tolerating
	// succeeded too) is what makes this test able to catch a regression that
	// drops commandOutcome's skip_reason -> failed mapping.
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

// TestUntypedSlashStaysPrompt is the named-failure test for the typed
// gate itself: every non-typed source (including the empty/default one,
// which normalizes to "api") must leave a "/name" line as an ordinary
// prompt. A regression that resolved a command for an untyped source would
// let a peer box or an unverified API caller run a control command
// through plain text.
func TestUntypedSlashStaysPrompt(t *testing.T) {
	for _, src := range []string{"", "api", "schedule", "cross_box"} {
		t.Run(src, func(t *testing.T) {
			prov := newCapturingProvider(asstTurn("ok"))
			h := newHarness(t, prov)
			id := h.createSession("test/m1")

			body := map[string]any{"parts": []map[string]string{{"type": "text", "text": "/model x/y"}}}
			if src != "" {
				body["source"] = src
			}
			resp, data := h.do("POST", "/session/"+id+"/prompt_async", body)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
			}
			h.waitIdle(id)

			users := h.userMessages(id)
			if len(users) != 1 || users[0].Parts.Text() != "/model x/y" {
				t.Fatalf("user messages = %+v, want one with text /model x/y", users)
			}
			if cmds := h.sessionDirect(id).Commands(); len(cmds) != 0 {
				t.Errorf("Commands() = %+v, want none", cmds)
			}
		})
	}
}

// TestUntypedCompactStaysPrompt pins the typed-only rule for "/compact":
// resolvePromptCommand only resolves a typed "/compact" (rule "typed
// only"). A non-typed source (including the empty/default one) and a
// typed "//compact" escape must both reach Session.Prompt as ordinary
// model input, appending a literal "/compact" user message and writing no
// CommandRecord — never compacting through the engine itself. #319 added
// an engine-side exact-text match that fired regardless of source; this
// test would have failed against that match.
func TestUntypedCompactStaysPrompt(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{name: "empty source", body: map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "/compact"}},
		}},
		{name: "api source", body: map[string]any{
			"parts":  []map[string]string{{"type": "text", "text": "/compact"}},
			"source": "api",
		}},
		{name: "typed escaped //compact", body: map[string]any{
			"parts":  []map[string]string{{"type": "text", "text": "//compact"}},
			"source": "typed",
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			prov := newCapturingProvider(asstTurn("ok"))
			h := newHarness(t, prov)
			id := h.createSession("test/m1")

			resp, data := h.do("POST", "/session/"+id+"/prompt_async", tt.body)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
			}
			h.waitIdle(id)

			users := h.userMessages(id)
			if len(users) != 1 || users[0].Parts.Text() != "/compact" {
				t.Fatalf("user messages = %+v, want one with text /compact", users)
			}
			if cmds := h.sessionDirect(id).Commands(); len(cmds) != 0 {
				t.Errorf("Commands() = %+v, want none", cmds)
			}
		})
	}
}

// TestTypedEscapedSlashSendsLiteral proves Resolve's rule 3 ("//name" is
// the literal text "/name") only fires for a typed source, and that an
// escaped line becomes an ordinary user message either way — never a
// command.
func TestTypedEscapedSlashSendsLiteral(t *testing.T) {
	prov := newCapturingProvider(asstTurn("ok"), asstTurn("ok2"))
	h := newHarness(t, prov)

	typedID := h.createSession("test/m1")
	resp, data := h.do("POST", "/session/"+typedID+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "//model x"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("typed prompt_async status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(typedID)

	plainID := h.createSession("test/m1")
	resp, data = h.do("POST", "/session/"+plainID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "//model x"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("plain prompt_async status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(plainID)

	typedUsers := h.userMessages(typedID)
	if len(typedUsers) != 1 || typedUsers[0].Parts.Text() != "/model x" {
		t.Fatalf("typed //model x -> %+v, want one user message with text /model x", typedUsers)
	}
	plainUsers := h.userMessages(plainID)
	if len(plainUsers) != 1 || plainUsers[0].Parts.Text() != "//model x" {
		t.Fatalf("non-typed //model x -> %+v, want one user message with text //model x", plainUsers)
	}
}

// TestUnknownTypedCommandStaysPrompt proves an unrecognized "/name" is
// never treated as a bad command: it stays an ordinary prompt, unchanged,
// and writes no command record — Resolve's UnknownCommandError path.
func TestUnknownTypedCommandStaysPrompt(t *testing.T) {
	prov := newCapturingProvider(asstTurn("ok"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/cost"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 || users[0].Parts.Text() != "/cost" {
		t.Fatalf("user messages = %+v, want one with text /cost", users)
	}
	if cmds := h.sessionDirect(id).Commands(); len(cmds) != 0 {
		t.Errorf("Commands() = %+v, want none", cmds)
	}
}

// TestTypedCommandBadArgsFailsWithoutPrompt proves a known command with an
// unparsable argument line records a single "failed" record carrying
// Resolve's own error text verbatim, and never appends a user message.
func TestTypedCommandBadArgsFailsWithoutPrompt(t *testing.T) {
	prov := newCapturingProvider()
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/compact abc"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	body := decodeCommandResponse(t, data)
	if body.Command.Status != "failed" {
		t.Fatalf("command.status = %q, want failed", body.Command.Status)
	}

	cmds := h.sessionDirect(id).Commands()
	if len(cmds) != 1 {
		t.Fatalf("Commands() = %+v, want 1", cmds)
	}
	want := `command: /compact keep_turns must be a number, got "abc"`
	if cmds[0].Text != want {
		t.Errorf("text = %q, want %q", cmds[0].Text, want)
	}
	if cmds[0].Name != "compact" {
		t.Errorf("name = %q, want compact", cmds[0].Name)
	}
	if len(h.userMessages(id)) != 0 {
		t.Errorf("user messages = %+v, want none", h.userMessages(id))
	}
}

// TestTypedCommandWithAttachmentFails proves a resolved command carrying
// an attachment part fails closed — "nothing ran" — rather than silently
// dropping the attachment or the command name, and leaves the session's
// model untouched.
func TestTypedCommandWithAttachmentFails(t *testing.T) {
	prov := newCapturingProvider()
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []any{
			map[string]string{"type": "text", "text": "/model a/b"},
			attachmentPart("image/png", testPNG(t)),
		},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	body := decodeCommandResponse(t, data)
	if body.Command.Status != "failed" {
		t.Fatalf("command.status = %q, want failed", body.Command.Status)
	}

	cmds := h.sessionDirect(id).Commands()
	if len(cmds) != 1 {
		t.Fatalf("Commands() = %+v, want 1", cmds)
	}
	want := "/model takes no attachments; nothing ran"
	if cmds[0].Text != want {
		t.Errorf("text = %q, want %q", cmds[0].Text, want)
	}

	sess := h.getSessionJSON(id)
	if sess.Model.String() != "test/m1" {
		t.Errorf("model = %q, want unchanged test/m1", sess.Model.String())
	}
}

// TestTypedFrontendCommandUnsupported proves every command serve mode does
// not resolve — a frontend command (including one reached by alias) and a
// control command with no serve-mode route — answers "unsupported" with
// the exact typed name, never the canonical registry name.
func TestTypedFrontendCommandUnsupported(t *testing.T) {
	for _, line := range []string{"/new", "/clear", "/queue-clear"} {
		t.Run(line, func(t *testing.T) {
			prov := newCapturingProvider()
			h := newHarness(t, prov)
			id := h.createSession("test/m1")

			resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
				"parts":  []map[string]string{{"type": "text", "text": line}},
				"source": "typed",
			})
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
			}
			body := decodeCommandResponse(t, data)
			if body.Command.Status != "unsupported" {
				t.Fatalf("command.status = %q, want unsupported", body.Command.Status)
			}

			cmds := h.sessionDirect(id).Commands()
			if len(cmds) != 1 {
				t.Fatalf("Commands() = %+v, want 1", cmds)
			}
			want := fmt.Sprintf("%s is not available in this client", line)
			if cmds[0].Text != want {
				t.Errorf("text = %q, want %q", cmds[0].Text, want)
			}
		})
	}
}

// TestMidTurnCompactRefusedNotQueued proves a command whose Spec is not
// AvailableDuringTask is refused outright while a turn runs — never
// silently queued behind it (unlike an ordinary busy prompt) — while a
// command that IS available during a task (here, /model) still dispatches
// and succeeds in the same window.
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

// TestEnqueueCommandSeqRunsOnce proves a typed command dispatched through
// POST /enqueue shares that route's own idempotency contract: a repeated
// seq answers "duplicate" and never records or dispatches a second time.
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

// TestCommandRouteErrorBecomesFailed proves a route call that itself 4xxs
// (here, POST /session/{id}/model rejecting an unconfigured provider)
// becomes a "failed" record carrying that route's own error text
// verbatim, not a generic message.
func TestCommandRouteErrorBecomesFailed(t *testing.T) {
	prov := newCapturingProvider()
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/model nope/x"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	accepted := sse.waitFor(t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("first command event = %+v, want accepted", accepted.Command)
	}
	terminal := sse.waitFor(t, "command")
	if terminal.Command == nil || terminal.Command.Status != message.CommandFailed {
		t.Fatalf("terminal command event = %+v, want failed", terminal.Command)
	}
	want := `provider "nope" is not configured`
	if terminal.Command.Text != want {
		t.Errorf("text = %q, want %q", terminal.Command.Text, want)
	}
}

// TestTypedCommandOnAllThreeRoutes proves the same resolution runs
// identically through every prompt-landing route: each answers a
// "command" response and records a "succeeded" terminal record carrying a
// result.
func TestTypedCommandOnAllThreeRoutes(t *testing.T) {
	cases := []struct {
		name string
		post func(h *harness, id string) (*http.Response, []byte)
	}{
		{"prompt_async", func(h *harness, id string) (*http.Response, []byte) {
			return h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
				"parts": []map[string]string{{"type": "text", "text": "/status"}}, "source": "typed",
			})
		}},
		{"send", func(h *harness, id string) (*http.Response, []byte) {
			return h.do("POST", "/session/"+id+"/send", map[string]any{
				"text": "/status", "source": "typed",
			})
		}},
		{"enqueue", func(h *harness, id string) (*http.Response, []byte) {
			return h.do("POST", "/session/"+id+"/enqueue", map[string]any{
				"parts": []map[string]string{{"type": "text", "text": "/status"}}, "seq": int64(1), "source": "typed",
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := newCapturingProvider()
			h := newHarness(t, prov)
			id := h.createSession("test/m1")
			sse := h.openSSE("?from=0", "")

			resp, data := tc.post(h, id)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("%s status %d: %s", tc.name, resp.StatusCode, data)
			}
			body := decodeCommandResponse(t, data)
			if body.Status != "command" {
				t.Fatalf("status = %q, want command", body.Status)
			}

			accepted := sse.waitFor(t, "command")
			if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
				t.Fatalf("first command event = %+v, want accepted", accepted.Command)
			}
			terminal := sse.waitFor(t, "command")
			if terminal.Command == nil || terminal.Command.Status != message.CommandSucceeded {
				t.Fatalf("terminal command event = %+v, want succeeded", terminal.Command)
			}
			if len(terminal.Command.Result) == 0 {
				t.Error("result is empty, want the /status route's body")
			}

			// A terminal status update for an already-folded ID must carry
			// the SAME CreatedAt/AfterMessageID the accepted record minted,
			// never a zero created_at or an emptied anchor.
			if terminal.Command.CreatedAt.IsZero() {
				t.Error("terminal command event has a zero created_at")
			}
			if !terminal.Command.CreatedAt.Equal(accepted.Command.CreatedAt) {
				t.Errorf("terminal command event CreatedAt = %v, want %v (the accepted record's own)",
					terminal.Command.CreatedAt, accepted.Command.CreatedAt)
			}
			if terminal.Command.AfterMessageID != accepted.Command.AfterMessageID {
				t.Errorf("terminal command event AfterMessageID = %q, want %q (the accepted record's own)",
					terminal.Command.AfterMessageID, accepted.Command.AfterMessageID)
			}
		})
	}
}

// TestStatusResultTruncatedOverCap is the unit-level test for
// commandOutcome's 16 KiB cap: a body over the cap is reported succeeded
// with ResultTruncated true and no Result, never a partially-copied body.
func TestStatusResultTruncatedOverCap(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 17<<10)
	status, _, result, truncated := commandOutcome(command.OpStatus, "status", http.StatusOK, body, false, true)
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

// TestCommandResponseWriterCapsBufferedBody: a handler that writes an
// unbounded 2xx body — GET /session/{id}/queue serializing every queued
// prompt, for instance — must never grow commandResponseWriter's buffer past
// commandResultCap+1 bytes. Failure: the buffer grows to the full 1 MiB
// written, proving the cap was applied only after the fact in commandOutcome
// rather than in the writer itself.
func TestCommandResponseWriterCapsBufferedBody(t *testing.T) {
	cw := newCommandResponseWriter(command.OpQueueList)
	chunk := bytes.Repeat([]byte("a"), 4096)
	total := 1 << 20 // 1 MiB, written in several calls
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

	status, _, result, truncated := commandOutcome(command.OpQueueList, "queue", cw.code, cw.body.Bytes(), false, true)
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

// TestCommandOutcomeInvalidBodyOmitsResult: a non-JSON 2xx body must never
// become Result. Every current handler calls writeJSON, so this cannot
// happen today, but json.RawMessage validates its bytes when the record it
// sits inside is marshaled — an invalid Result would fail that marshal and
// strand the command "accepted" until the next boot. Failure: Result holds
// bytes that are not valid JSON.
func TestCommandOutcomeInvalidBodyOmitsResult(t *testing.T) {
	status, text, result, truncated := commandOutcome(command.OpStatus, "status", http.StatusOK, []byte("not json"), false, true)
	if status != message.CommandSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if text != "/status succeeded" {
		t.Errorf("text = %q, want %q", text, "/status succeeded")
	}
	if result != nil {
		t.Errorf("result = %q, want nil (invalid JSON must never be journaled as a result)", result)
	}
	if truncated {
		t.Error("truncated = true, want false")
	}
}

// TestCommandOutcomeDrainingNon2xxIsInterrupted: a route's non-2xx result
// while the server is draining maps to interrupted with the drain wording,
// never failed — the one status/text row nothing else in this package
// exercised.
func TestCommandOutcomeDrainingNon2xxIsInterrupted(t *testing.T) {
	status, text, result, truncated := commandOutcome(command.OpStatus, "status", http.StatusInternalServerError, []byte(`{"error":"boom"}`), true, true)
	if status != message.CommandInterrupted {
		t.Errorf("status = %q, want interrupted", status)
	}
	want := "harness stopped before /status finished; it will not run again"
	if text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if result != nil || truncated {
		t.Errorf("result = %q truncated = %v, want nil/false", result, truncated)
	}
}

// TestCommandOutcome409RefusedByAvailableDuringTask: a 409 maps to refused
// only for an Op whose spec is not availableDuringTask; the same 409 for an
// availableDuringTask Op keeps today's failed mapping with the route's own
// error text. commandOutcome keys this off the caller-supplied flag alone,
// never the error string, since both 409 causes (this session's own turn,
// or another session's turn holding the workdir) must map the same way.
func TestCommandOutcome409RefusedByAvailableDuringTask(t *testing.T) {
	tests := []struct {
		name                string
		availableDuringTask bool
		wantStatus          message.CommandStatus
		wantText            string
	}{
		{
			name:                "not available during task is refused",
			availableDuringTask: false,
			wantStatus:          message.CommandRefused,
			wantText:            "/compact cannot run while a turn is running; send it again after the turn ends",
		},
		{
			name:                "available during task keeps failed",
			availableDuringTask: true,
			wantStatus:          message.CommandFailed,
			wantText:            "session is busy with another prompt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, text, result, truncated := commandOutcome(command.OpCompact, "compact", http.StatusConflict,
				[]byte(`{"error":"session is busy with another prompt"}`), false, tt.availableDuringTask)
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if result != nil || truncated {
				t.Errorf("result = %q truncated = %v, want nil/false", result, truncated)
			}
		})
	}
}

// TestServeOpHandlersCoverSupportedOps keeps serveOpHandlers and opRoutes
// total over every Op serveModeOps marks supported, and proves the one
// deliberately unsupported control Op (queue_list's sibling, queue_clear)
// carries no dispatch entry — a silent gap here would let
// resolvePromptCommand's accepted branch panic on a nil map lookup instead
// of failing a build-time-visible test.
func TestServeOpHandlersCoverSupportedOps(t *testing.T) {
	for op, supported := range serveModeOps {
		if !supported {
			continue
		}
		if _, ok := serveOpHandlers[op]; !ok {
			t.Errorf("serveOpHandlers[%q] missing for a supported op", op)
		}
		if _, ok := opRoutes[op]; !ok {
			t.Errorf("opRoutes[%q] missing for a supported op", op)
		}
	}
	if _, ok := serveOpHandlers[command.OpQueueClear]; ok {
		t.Error("serveOpHandlers[OpQueueClear] present, want absent")
	}
}

// TestTypedCommandRefusedWhileDraining: admitCommand's 503 refusal for a
// dispatchable typed command while the server drains — driven directly by
// setting draining, the same admission claimForPrompt's own prompt turns
// get. Failure: a command still dispatches during drain, or a record lands
// on the session despite the 503.
func TestTypedCommandRefusedWhileDraining(t *testing.T) {
	prov := newCapturingProvider()
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	h.srv.mu.Lock()
	h.srv.draining = true
	h.srv.mu.Unlock()

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "/compact"}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("prompt_async status %d, want 503: %s", resp.StatusCode, data)
	}

	sess := h.sessionDirect(id)
	if cmds := sess.Commands(); len(cmds) != 0 {
		t.Fatalf("Commands() = %+v, want none: a 503 refusal must not record anything", cmds)
	}
}
