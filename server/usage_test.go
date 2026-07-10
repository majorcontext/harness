package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// usageJSONForTest mirrors the openapi Usage shape.
type usageJSONForTest struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	Messages         int `json:"messages"`
	LastInputTokens  int `json:"last_input_tokens,omitempty"`
}

type sessionJSONForTest struct {
	ID             string           `json:"id"`
	Messages       int              `json:"messages"`
	Usage          usageJSONForTest `json:"usage"`
	LastActivityAt time.Time        `json:"last_activity_at"`
}

func withUsageTurn(text string, in, out int) []provider.Event {
	msg := &message.Message{ID: message.ProviderCallID("m", text, 12), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: in, OutputTokens: out}}}
}

// TestSessionUsageSurfacedOnGet is the red-first test for issue #62 layer 2:
// GET /session/{id} must surface cumulative usage, message count, and the
// most recent turn's input tokens, so an orchestrator can rotate a session
// before it hits the provider's context cliff instead of learning about it
// from a failed prompt.
func TestSessionUsageSurfacedOnGet(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		withUsageTurn("first", 100, 20),
		withUsageTurn("second", 150, 30),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	sse.collectUntilIdle(t)

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var sess sessionJSONForTest
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Usage.InputTokens != 100 || sess.Usage.OutputTokens != 20 {
		t.Errorf("Usage = %+v, want cumulative input=100 output=20", sess.Usage)
	}
	if sess.Usage.LastInputTokens != 100 {
		t.Errorf("Usage.LastInputTokens = %d, want 100 (most recent turn)", sess.Usage.LastInputTokens)
	}
	if sess.Usage.Messages != sess.Messages {
		t.Errorf("Usage.Messages = %d, want it to match top-level Messages %d", sess.Usage.Messages, sess.Messages)
	}
	if sess.LastActivityAt.IsZero() {
		t.Error("LastActivityAt is zero after a completed turn")
	}

	// A second turn advances both cumulative usage and last_input_tokens.
	// Reuse the ORIGINAL stream: it has already consumed through turn 1's
	// idle, so the next idle it sees is genuinely turn 2's. A fresh
	// ?from=0 stream would REPLAY turn 1's journaled idle and release the
	// wait before turn 2 commits its usage — the intermittent
	// input_tokens=100-instead-of-250 failure seen under load.
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "again"}},
	})
	sse.collectUntilIdle(t)

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Usage.InputTokens != 250 || sess.Usage.OutputTokens != 50 {
		t.Errorf("Usage after 2nd turn = %+v, want cumulative input=250 output=50", sess.Usage)
	}
	if sess.Usage.LastInputTokens != 150 {
		t.Errorf("Usage.LastInputTokens after 2nd turn = %d, want 150", sess.Usage.LastInputTokens)
	}
}

// TestSessionUsageSurfacedOnListAndStatus covers the list and /status
// surfaces the issue explicitly calls out alongside GET /session/{id}.
func TestSessionUsageSurfacedOnListAndStatus(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{withUsageTurn("hi", 42, 7)}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	sse.collectUntilIdle(t)

	resp, data := h.do("GET", "/session", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /session status %d: %s", resp.StatusCode, data)
	}
	var list []sessionJSONForTest
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range list {
		if s.ID == id {
			found = true
			if s.Usage.InputTokens != 42 {
				t.Errorf("list entry usage = %+v, want input 42", s.Usage)
			}
		}
	}
	if !found {
		t.Fatalf("session %s missing from list", id)
	}

	resp, data = h.do("GET", "/session/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /session/status status %d: %s", resp.StatusCode, data)
	}
	var statuses map[string]struct {
		Usage usageJSONForTest `json:"usage"`
	}
	if err := json.Unmarshal(data, &statuses); err != nil {
		t.Fatal(err)
	}
	entry, ok := statuses[id]
	if !ok {
		t.Fatalf("session %s missing from status map", id)
	}
	if entry.Usage.InputTokens != 42 || entry.Usage.OutputTokens != 7 {
		t.Errorf("status entry usage = %+v, want input=42 output=7", entry.Usage)
	}
}

// TestSessionUsageSurfacedForNonResidentSession exercises the on-disk path
// (server restart / evicted session) for both GET /session/{id} and
// /session/status: usage must not silently reset to zero just because the
// session isn't resident in this process.
func TestSessionUsageSurfacedForNonResidentSession(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{withUsageTurn("hi", 42, 7)}}
	h := newHarnessDir(t, dir, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	sse.collectUntilIdle(t)

	// Restart the process (fresh Server over the same directory): the
	// session is no longer resident.
	h2 := newHarnessDir(t, dir, prov)

	resp, data := h2.do("GET", "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var sess sessionJSONForTest
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Usage.InputTokens != 42 {
		t.Errorf("non-resident Usage = %+v, want input 42 (survives restart)", sess.Usage)
	}
	if sess.LastActivityAt.IsZero() {
		t.Error("non-resident LastActivityAt is zero")
	}

	resp, data = h2.do("GET", "/session/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /session/status status %d: %s", resp.StatusCode, data)
	}
	var statuses map[string]struct {
		Usage usageJSONForTest `json:"usage"`
	}
	if err := json.Unmarshal(data, &statuses); err != nil {
		t.Fatal(err)
	}
	entry, ok := statuses[id]
	if !ok {
		t.Fatalf("session %s missing from status map", id)
	}
	if entry.Usage.InputTokens != 42 {
		t.Errorf("non-resident status usage = %+v, want input 42", entry.Usage)
	}
}
