package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func (h *harness) dispatchTypedCompactAndWaitTerminal(id, line string) *message.CommandRecord {
	h.t.Helper()
	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": line}}, "source": "typed",
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

func TestCompactCommandResultIsSlim(t *testing.T) {
	for _, tc := range []struct {
		name, summary string
		delegated     bool
	}{
		{name: "native", summary: "SUMMARY"},
		{name: "over cap", summary: strings.Repeat("SUMMARYTEXT ", 1700)},
		{name: "delegated", delegated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *harness
			var id, line string
			if tc.delegated {
				bin := buildFakeClaudeForServer(t)
				t.Setenv("FAKE_CLAUDE_MODE", "compact_turn")
				t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(t.TempDir(), "invocations.jsonl"))
				model := message.ModelRef{Provider: engine.ClaudeCodeProviderFamily, Model: "sonnet"}
				h = claudeCodeSwitchHarness(t, model, engine.ClaudeCodeConfig{BinaryPath: bin}, &scriptedProvider{name: "test"})
				id, line = h.createSession(""), "/compact"
			} else {
				prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
					compactAsstTurn("one", provider.Usage{InputTokens: 10}),
					compactAsstTurn("two", provider.Usage{InputTokens: 20}),
					compactAsstTurn("three", provider.Usage{InputTokens: 30}),
					compactAsstTurn(tc.summary, provider.Usage{InputTokens: 5}),
				}}
				h = newHarness(t, prov)
				id, line = h.createSession("test/m1"), "/compact 1"
				for _, prompt := range []string{"go1", "go2", "go3"} {
					h.promptAndWaitIdle(id, prompt)
				}
			}
			rec := h.dispatchTypedCompactAndWaitTerminal(id, line)
			if rec.Status != message.CommandSucceeded || rec.ResultTruncated {
				t.Fatalf("terminal = %+v, want succeeded without truncation", rec)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rec.Result, &raw); err != nil {
				t.Fatalf("Result not valid JSON: %v (%s)", err, rec.Result)
			}
			keys := []string{"turns_folded", "first_id", "last_id", "summary_id"}
			if tc.delegated {
				keys = []string{"claude_code_delegated"}
			}
			if len(raw) != len(keys) {
				t.Fatalf("Result keys = %v, want exactly %v", raw, keys)
			}
			for _, key := range keys {
				if _, ok := raw[key]; !ok {
					t.Errorf("Result missing %q: %s", key, rec.Result)
				}
			}
			if tc.delegated {
				if string(raw["claude_code_delegated"]) != "true" {
					t.Errorf("delegated result = %s, want true", rec.Result)
				}
				return
			}
			var out struct {
				TurnsFolded int    `json:"turns_folded"`
				FirstID     string `json:"first_id"`
				LastID      string `json:"last_id"`
				SummaryID   string `json:"summary_id"`
			}
			if err := json.Unmarshal(rec.Result, &out); err != nil {
				t.Fatal(err)
			}
			if out.TurnsFolded != 2 || out.FirstID == "" || out.LastID == "" || out.SummaryID == "" {
				t.Errorf("compact result = %+v, want 2 turns and nonempty ids", out)
			}
			if len(rec.Result) > commandResultCap {
				t.Errorf("Result size = %d, exceeds cap", len(rec.Result))
			}
			var msgs []message.Message
			_, data := h.do("GET", "/session/"+id+"/message", nil)
			if err := json.Unmarshal(data, &msgs); err != nil {
				t.Fatal(err)
			}
			if len(msgs) == 0 || msgs[0].ID != out.SummaryID {
				t.Errorf("summary_id = %q, history = %+v", out.SummaryID, msgs)
			}
		})
	}
}
