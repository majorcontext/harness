package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// transcriptCommandsResponse mirrors transcriptJSON for a test that decodes
// the bootstrap envelope's commands field as a client would.
type transcriptCommandsResponse struct {
	Messages   []message.Message       `json:"messages"`
	StreamFrom int64                   `json:"stream_from"`
	LiveFrom   int64                   `json:"live_from"`
	Seqs       []int64                 `json:"seqs"`
	Commands   []message.CommandRecord `json:"commands"`
}

// pageCommandsResponse mirrors messagePageJSON for a test that decodes the
// page envelope's commands field as a client would.
type pageCommandsResponse struct {
	Messages []message.Message       `json:"messages"`
	FirstSeq int                     `json:"first_seq"`
	LastSeq  int                     `json:"last_seq"`
	Total    int                     `json:"total"`
	HasMore  bool                    `json:"has_more"`
	Commands []message.CommandRecord `json:"commands"`
}

func getTranscriptCommands(t *testing.T, h *harness, id, query string) transcriptCommandsResponse {
	t.Helper()
	resp, data := h.do("GET", "/session/"+id+"/message?stream_from=1"+query, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message?stream_from=1%s = %d: %s", query, resp.StatusCode, data)
	}
	var got transcriptCommandsResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode transcript: %v (%s)", err, data)
	}
	return got
}

func getPageCommands(t *testing.T, h *harness, id, query string) pageCommandsResponse {
	t.Helper()
	resp, data := h.do("GET", "/session/"+id+"/message"+query, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message%s = %d: %s", query, resp.StatusCode, data)
	}
	var got pageCommandsResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode page: %v (%s)", err, data)
	}
	return got
}

// newTestCommandRecord builds a minimal succeeded command record, ready for
// RecordCommand: AfterMessageID and the timestamps are stamped by the
// engine at record time, from the session's own history and clock.
func newTestCommandRecord(name string) message.CommandRecord {
	return message.CommandRecord{
		ID:     engine.NewCommandID(),
		Line:   "/" + name,
		Name:   name,
		Source: message.PromptSourceTyped,
		Status: message.CommandSucceeded,
	}
}

func commandIDsOf(cmds []message.CommandRecord) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.ID)
	}
	return out
}

// sameCommandIDs checks fold order too, not just membership:
// CommandsInWindow promises fold order, and a duplicate can hide a missing
// record that plain membership would miss.
func sameCommandIDs(got, want []string) bool {
	return slices.Equal(got, want)
}

// fourTurnProvider is four scripted assistant turns: enough for a resident
// session or a cold journal to carry a command anchored mid-history, one
// anchored at the tail, and one anchored before the first message.
func fourTurnProvider() *scriptedProvider {
	return &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("one"), asstTurn("two"), asstTurn("three"), asstTurn("four"),
	}}
}

// TestTranscriptBootstrapCarriesCommands_Resident is Task 8's red-first test
// for the resident bootstrap path: GET /session/{id}/message?stream_from=1
// must carry every folded command anchored inside the returned window,
// including an empty-anchored command only when that window starts at the
// session's first message. Failure: a reload of a resident session loses
// every command outcome the console rendered before it.
func TestTranscriptBootstrapCarriesCommands_Resident(t *testing.T) {
	h := newHarness(t, fourTurnProvider())
	id := h.createSession("")
	sess := h.srv.residentSession(id)
	if sess == nil {
		t.Fatal("session not resident after creation -- test setup invariant broken")
	}

	// c0 is recorded before any message exists, so its anchor is "".
	c0 := newTestCommandRecord("status")
	if err := sess.RecordCommand(c0); err != nil {
		t.Fatalf("RecordCommand c0: %v", err)
	}

	for _, text := range []string{"go1", "go2"} {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": text}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async(%q) = %d: %s", text, resp.StatusCode, data)
		}
		h.waitIdle(id)
	}
	// History now holds 4 messages (2 turns).

	c1 := newTestCommandRecord("thinking")
	if err := sess.RecordCommand(c1); err != nil {
		t.Fatalf("RecordCommand c1: %v", err)
	}

	for _, text := range []string{"go3", "go4"} {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": text}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async(%q) = %d: %s", text, resp.StatusCode, data)
		}
		h.waitIdle(id)
	}
	// History now holds 8 messages (4 turns).

	c2 := newTestCommandRecord("compact")
	if err := sess.RecordCommand(c2); err != nil {
		t.Fatalf("RecordCommand c2: %v", err)
	}

	// Without limit: the window is the whole history, starting at its own
	// first message, so every command (including the empty-anchored c0)
	// belongs.
	full := getTranscriptCommands(t, h, id, "")
	if got := commandIDsOf(full.Commands); !sameCommandIDs(got, []string{c0.ID, c1.ID, c2.ID}) {
		t.Fatalf("full bootstrap commands = %v, want [c0 c1 c2]", got)
	}

	// With limit=2: the window narrows to the tail (the last 2 messages),
	// which does not start at history's first message. c0 (anchor "")
	// drops out because the window is no longer "from the first message";
	// c1 (anchored mid-history) drops out because its anchor is not in the
	// tail; only c2 (anchored at the last message) survives.
	windowed := getTranscriptCommands(t, h, id, "&limit=2")
	if got := commandIDsOf(windowed.Commands); !sameCommandIDs(got, []string{c2.ID}) {
		t.Fatalf("windowed bootstrap commands = %v, want [c2]", got)
	}
}

// TestTranscriptBootstrapCarriesCommands_Cold is TestTranscriptBootstrapCarriesCommands_Resident's
// cold counterpart: a session this process has never made resident, read
// through both transcriptSyncedThrough (no limit) and coldWindowedBootstrap
// (limit), must carry the identical commands its resident twin would.
// Failure: a hibernated box's snapshot silently drops every command
// outcome on reload.
func TestTranscriptBootstrapCarriesCommands_Cold(t *testing.T) {
	dir := t.TempDir()
	prov := fourTurnProvider()
	sess := engine.NewSession(engine.Config{
		Providers:  provider.Registry{prov.name: prov},
		Model:      message.ModelRef{Provider: prov.name, Model: "m1"},
		SessionDir: dir,
		WorkDir:    dir,
	})

	c0 := newTestCommandRecord("status")
	if err := sess.RecordCommand(c0); err != nil {
		t.Fatalf("RecordCommand c0: %v", err)
	}
	for _, text := range []string{"ask1", "ask2"} {
		if _, err := sess.Prompt(context.Background(), text); err != nil {
			t.Fatalf("Prompt(%q): %v", text, err)
		}
	}
	c1 := newTestCommandRecord("thinking")
	if err := sess.RecordCommand(c1); err != nil {
		t.Fatalf("RecordCommand c1: %v", err)
	}
	for _, text := range []string{"ask3", "ask4"} {
		if _, err := sess.Prompt(context.Background(), text); err != nil {
			t.Fatalf("Prompt(%q): %v", text, err)
		}
	}
	c2 := newTestCommandRecord("compact")
	if err := sess.RecordCommand(c2); err != nil {
		t.Fatalf("RecordCommand c2: %v", err)
	}
	if err := sess.PersistErr(); err != nil {
		t.Fatalf("PersistErr: %v", err)
	}

	h := newHarnessDir(t, dir, prov)
	if h.srv.residentSession(sess.ID) != nil {
		t.Fatal("session unexpectedly resident -- test setup invariant broken")
	}

	full := getTranscriptCommands(t, h, sess.ID, "")
	if got := commandIDsOf(full.Commands); !sameCommandIDs(got, []string{c0.ID, c1.ID, c2.ID}) {
		t.Fatalf("cold full bootstrap commands = %v, want [c0 c1 c2]", got)
	}
	if h.srv.residentSession(sess.ID) != nil {
		t.Fatal("the unwindowed bootstrap read unexpectedly made the session resident")
	}

	windowed := getTranscriptCommands(t, h, sess.ID, "&limit=2")
	if got := commandIDsOf(windowed.Commands); !sameCommandIDs(got, []string{c2.ID}) {
		t.Fatalf("cold windowed bootstrap commands = %v, want [c2]", got)
	}
	if h.srv.residentSession(sess.ID) != nil {
		t.Fatal("coldWindowedBootstrap unexpectedly made the session resident")
	}
}

// TestMessagePageCarriesCommands is Task 8's red-first test for
// GET /session/{id}/message?before_seq=N&limit=K: each page must carry
// exactly the folded commands anchored inside its own window, from
// engine.ReadMessagePage's own MessagePage.Commands.
func TestMessagePageCarriesCommands(t *testing.T) {
	dir := t.TempDir()
	prov := fourTurnProvider()
	sess := engine.NewSession(engine.Config{
		Providers:  provider.Registry{prov.name: prov},
		Model:      message.ModelRef{Provider: prov.name, Model: "m1"},
		SessionDir: dir,
		WorkDir:    dir,
	})

	c0 := newTestCommandRecord("status")
	if err := sess.RecordCommand(c0); err != nil {
		t.Fatalf("RecordCommand c0: %v", err)
	}
	for _, text := range []string{"ask1", "ask2"} {
		if _, err := sess.Prompt(context.Background(), text); err != nil {
			t.Fatalf("Prompt(%q): %v", text, err)
		}
	}
	c1 := newTestCommandRecord("thinking")
	if err := sess.RecordCommand(c1); err != nil {
		t.Fatalf("RecordCommand c1: %v", err)
	}
	for _, text := range []string{"ask3", "ask4"} {
		if _, err := sess.Prompt(context.Background(), text); err != nil {
			t.Fatalf("Prompt(%q): %v", text, err)
		}
	}
	c2 := newTestCommandRecord("compact")
	if err := sess.RecordCommand(c2); err != nil {
		t.Fatalf("RecordCommand c2: %v", err)
	}
	if err := sess.PersistErr(); err != nil {
		t.Fatalf("PersistErr: %v", err)
	}

	h := newHarnessDir(t, dir, prov)

	// History: m1..m8. c0 anchors "" (before m1), c1 anchors m4, c2
	// anchors m8. before_seq=5&limit=5 pages m1..m4.
	older := getPageCommands(t, h, sess.ID, "?before_seq=5&limit=5")
	if got := commandIDsOf(older.Commands); !sameCommandIDs(got, []string{c0.ID, c1.ID}) {
		t.Fatalf("older page commands = %v, want [c0 c1] (anchored \"\" and m4, page starts at m1)", got)
	}

	// The newest page (before_seq=0&limit=3) pages m6..m8: c2 (anchor m8)
	// belongs; c0 and c1 do not, and the page does not start at m1.
	newest := getPageCommands(t, h, sess.ID, "?before_seq=0&limit=3")
	if got := commandIDsOf(newest.Commands); !sameCommandIDs(got, []string{c2.ID}) {
		t.Fatalf("newest page commands = %v, want [c2] (anchored m8)", got)
	}
}

// TestMessagePageFallbackCarriesCommands is Task 8's red-first test for
// messagePageFallback: a page answered from resident history (no readable
// journal) must still carry the live session's own folded commands, from
// engine.CommandsInWindow over the window messagePageFallback computes
// itself.
func TestMessagePageFallbackCarriesCommands(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one")}})
	id := h.createSession("")
	sess := h.srv.residentSession(id)
	if sess == nil {
		t.Fatal("session not resident after creation -- test setup invariant broken")
	}

	c0 := newTestCommandRecord("status")
	if err := sess.RecordCommand(c0); err != nil {
		t.Fatalf("RecordCommand c0: %v", err)
	}

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	c1 := newTestCommandRecord("thinking")
	if err := sess.RecordCommand(c1); err != nil {
		t.Fatalf("RecordCommand c1: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, id+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, id+".index.json")); err != nil {
		t.Fatal(err)
	}

	page := getPageCommands(t, h, id, "?limit=10")
	if got := commandIDsOf(page.Commands); !sameCommandIDs(got, []string{c0.ID, c1.ID}) {
		t.Fatalf("fallback page commands = %v, want [c0 c1]", got)
	}
}

// TestUnparameterizedMessagesUnchangedWithCommands pins the compatibility
// promise the brief requires: a caller that asks for no page keeps getting
// the bare array of the whole history, unchanged, even when the session
// carries folded commands -- the bare-array shape has no room for a
// "commands" field, so this is unaffected by this task by construction.
func TestUnparameterizedMessagesUnchangedWithCommands(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one")}})
	id := h.createSession("")
	sess := h.srv.residentSession(id)
	if sess == nil {
		t.Fatal("session not resident after creation -- test setup invariant broken")
	}
	if err := sess.RecordCommand(newTestCommandRecord("status")); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	resp, data = h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message = %d: %s", resp.StatusCode, data)
	}
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) == 0 || trimmed[0] != '[' {
		t.Fatalf("unparameterized response must stay a bare JSON array, got %q", trimmed)
	}
	var msgs []message.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatalf("unparameterized response must decode as a bare array: %v (%s)", err, data)
	}
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want 2", len(msgs))
	}
}
