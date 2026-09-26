package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// dispatchTypedCompactAndWaitTerminal posts a typed "/compact" (or
// "/compact <keep_turns>") through prompt_async and returns the terminal
// CommandRecord it journals, the same path a real client's typed slash
// command takes (resolvePromptCommand -> runCommand -> handleCompact, see
// command_dispatch.go).
func (h *harness) dispatchTypedCompactAndWaitTerminal(id, line string) *message.CommandRecord {
	h.t.Helper()
	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": line}},
		"source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	accepted := sse.waitFor(h.t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		h.t.Fatalf("first command event = %+v, want accepted", accepted.Command)
	}
	terminal := sse.waitFor(h.t, "command")
	if terminal.Command == nil {
		h.t.Fatal("terminal command event carries no Command")
	}
	return terminal.Command
}

// TestCompactCommandResultIsSlimNativeSummary is the red-first test for the
// controller ruling: a typed "/compact" that folds turns on a native session
// must record a CommandRecord.Result equal to exactly
// {"turns_folded","first_id","last_id","summary_id"} — never the route's
// full compactResponseJSON body, which duplicates the fold's own summary
// message already durable in history.
func TestCompactCommandResultIsSlimNativeSummary(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
		compactAsstTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")
	h.promptAndWaitIdle(id, "go3")

	rec := h.dispatchTypedCompactAndWaitTerminal(id, "/compact 1")
	if rec.Status != message.CommandSucceeded {
		t.Fatalf("terminal status = %q, want succeeded (text=%q)", rec.Status, rec.Text)
	}
	if rec.ResultTruncated {
		t.Error("ResultTruncated = true, want false")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Result, &raw); err != nil {
		t.Fatalf("Result not valid JSON: %v (%s)", err, rec.Result)
	}
	wantKeys := []string{"turns_folded", "first_id", "last_id", "summary_id"}
	if len(raw) != len(wantKeys) {
		t.Fatalf("Result has %d keys, want exactly %v: %s", len(raw), wantKeys, rec.Result)
	}
	for _, k := range wantKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("Result missing key %q: %s", k, rec.Result)
		}
	}

	var out struct {
		TurnsFolded int    `json:"turns_folded"`
		FirstID     string `json:"first_id"`
		LastID      string `json:"last_id"`
		SummaryID   string `json:"summary_id"`
	}
	if err := json.Unmarshal(rec.Result, &out); err != nil {
		t.Fatalf("decode Result: %v (%s)", err, rec.Result)
	}
	if out.TurnsFolded != 2 {
		t.Errorf("turns_folded = %d, want 2 (the fold this session ran)", out.TurnsFolded)
	}
	if out.FirstID == "" || out.LastID == "" {
		t.Errorf("first_id/last_id empty: %+v", out)
	}
	if out.SummaryID == "" {
		t.Fatalf("summary_id empty: %+v", out)
	}

	var msgs []message.Message
	_, data := h.do("GET", "/session/"+id+"/message", nil)
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].ID != out.SummaryID {
		t.Errorf("summary_id = %q, want history's own summary message id (messages[0].ID = %q)", out.SummaryID, msgs[0].ID)
	}
}

// TestCompactCommandResultStaysSlimOverCap is the red-first test for the
// cap-independence half of the controller ruling: a summary whose wire body
// exceeds commandResultCap (16 KiB) must still yield the same slim Result,
// never a dropped/truncated one — the capture writer keeps the whole body
// for OpCompact precisely so this parse can still succeed.
func TestCompactCommandResultStaysSlimOverCap(t *testing.T) {
	big := ""
	for len(big) < 20000 {
		big += "SUMMARYTEXT "
	}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
		compactAsstTurn(big, provider.Usage{InputTokens: 5}),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	h.promptAndWaitIdle(id, "go1")
	h.promptAndWaitIdle(id, "go2")
	h.promptAndWaitIdle(id, "go3")

	rec := h.dispatchTypedCompactAndWaitTerminal(id, "/compact 1")
	if rec.Status != message.CommandSucceeded {
		t.Fatalf("terminal status = %q, want succeeded (text=%q)", rec.Status, rec.Text)
	}
	if rec.ResultTruncated {
		t.Error("ResultTruncated = true, want false (the slim object never depends on commandResultCap)")
	}
	if len(rec.Result) == 0 {
		t.Fatal("Result is empty, want the slim object even though the route body exceeds commandResultCap")
	}
	var out struct {
		TurnsFolded int    `json:"turns_folded"`
		SummaryID   string `json:"summary_id"`
	}
	if err := json.Unmarshal(rec.Result, &out); err != nil {
		t.Fatalf("Result not valid JSON: %v (%s)", err, rec.Result)
	}
	if out.TurnsFolded != 2 {
		t.Errorf("turns_folded = %d, want 2", out.TurnsFolded)
	}
	if out.SummaryID == "" {
		t.Error("summary_id empty")
	}
	if len(rec.Result) > commandResultCap {
		t.Errorf("Result is %d bytes, want the slim object well under commandResultCap", len(rec.Result))
	}
}

// TestCompactCommandResultDelegatedIsSlim is the red-first test for the
// delegated lane's controller ruling: Result must be exactly
// {"claude_code_delegated":true}, never a body carrying turns_folded (always
// 0 on this lane, since no native fold ran at all).
func TestCompactCommandResultDelegatedIsSlim(t *testing.T) {
	bin := buildFakeClaudeForServer(t)
	t.Setenv("FAKE_CLAUDE_MODE", "compact_turn")
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(t.TempDir(), "invocations.jsonl"))

	claudeModel := message.ModelRef{Provider: engine.ClaudeCodeProviderFamily, Model: "sonnet"}
	nativeProv := &scriptedProvider{name: "test"}
	h := claudeCodeSwitchHarness(t, claudeModel, engine.ClaudeCodeConfig{BinaryPath: bin}, nativeProv)
	id := h.createSession("")

	rec := h.dispatchTypedCompactAndWaitTerminal(id, "/compact")
	if rec.Status != message.CommandSucceeded {
		t.Fatalf("terminal status = %q, want succeeded (text=%q)", rec.Status, rec.Text)
	}
	if rec.ResultTruncated {
		t.Error("ResultTruncated = true, want false")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Result, &raw); err != nil {
		t.Fatalf("Result not valid JSON: %v (%s)", err, rec.Result)
	}
	if len(raw) != 1 {
		t.Fatalf("Result has %d keys, want exactly 1 (claude_code_delegated): %s", len(raw), rec.Result)
	}
	var out struct {
		ClaudeCodeDelegated bool `json:"claude_code_delegated"`
	}
	if err := json.Unmarshal(rec.Result, &out); err != nil {
		t.Fatalf("decode Result: %v (%s)", err, rec.Result)
	}
	if !out.ClaudeCodeDelegated {
		t.Fatalf("claude_code_delegated = false, want true: %s", rec.Result)
	}
}
