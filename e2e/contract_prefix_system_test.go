package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// declaredSystemRows is the disposition of the rows of the declared system prompt.
var declaredSystemRows = map[string]runtimeRow{
	"an_edited_prompt_file_is_not_read_again_after_a_restart":                   sameAsServe(),
	"a_compaction_between_turns_declares_an_edited_prompt_file":                 sameAsServe(),
	"a_compaction_between_turns_declares_an_edited_prompt_file_after_a_restart": sameAsServe(),
	"an_edit_of_a_prompt_file_inside_a_turn_waits_for_a_compaction":             sameAsServe(),
}

const (
	ruleOne = "Rule one of the project.\n"
	ruleTwo = "Rule two of the project.\n"
)

func TestContractDeclaredSystem(t *testing.T) {
	text, summary := replyTo, gistSummary
	edit := writeFile{path: "AGENTS.md", body: ruleTwo}
	runScenarios(t, []scenario{
		{
			name:  "an_edited_prompt_file_is_not_read_again_after_a_restart",
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				writeFile{path: "AGENTS.md", body: ruleOne},
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				edit,
				restart{},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectSystem{req: 1, has: []string{"Rule one"}},
				expectSystem{req: 2, sameAs: 1, lacks: []string{"Rule two"}},
			},
		},
		{
			name: "a_compaction_between_turns_declares_an_edited_prompt_file", prefixBreaks: compactionBreak(),
			config: keepOneTurn,
			model:  []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3")},
			actions: []action{
				writeFile{path: "AGENTS.md", body: ruleOne},
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				edit,
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectSystem{req: 2, sameAs: 1, lacks: []string{"Rule two"}},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectSystem{req: 4, has: []string{"Rule two"}, lacks: []string{"Rule one"}},
			},
		},
		{
			name: "a_compaction_between_turns_declares_an_edited_prompt_file_after_a_restart", prefixBreaks: compactionBreak(),
			config: keepOneTurn,
			model:  []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), summary, text("three", "three", "3")},
			actions: []action{
				writeFile{path: "AGENTS.md", body: ruleOne},
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				edit,
				restart{},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectSystem{req: 2, sameAs: 1, lacks: []string{"Rule two"}},
				compact{as: "a"},
				restart{},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectSystem{req: 4, has: []string{"Rule two"}, lacks: []string{"Rule one"}},
			},
		},
		{
			name: "an_edit_of_a_prompt_file_inside_a_turn_waits_for_a_compaction", prefixBreaks: compactionBreak(),
			config: keepOneTurn,
			model: []harnesstest.Step{
				text("one", "one", "1"),
				{Name: "edit", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{withID("toolu_edit", ftBash("printf 'Rule two of the project.\\n' > AGENTS.md"))}}},
				{Name: "edited", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "2"}},
				summary, text("three", "three", "3"),
			},
			actions: []action{
				writeFile{path: "AGENTS.md", body: ruleOne},
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectSystem{req: 3, sameAs: 1, has: []string{"Rule one"}, lacks: []string{"Rule two"}},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectSystem{req: 5, has: []string{"Rule two"}},
			},
		},
	})
}

func TestContractDeclaredSystemChild(t *testing.T) {
	const body = "Read the files that the parent names."
	files := map[string]string{".agents/reader.md": "---\nname: reader\ndescription: Reads.\n---\n\n" + body + "\n"}
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_child_declares_the_body_of_its_agent_profile", func(t *testing.T) {
			t.Parallel()
			d, fake := startOn(t, h, runtimeWorkdir(t, files), nil, delegation("reader", childDone,
				harnesstest.Step{Name: "report", Match: harnesstest.LastUserText("child done"), Reply: harnesstest.Reply{Text: "parent done"}},
				harnesstest.Step{Name: "again", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "child again"}},
			)...)
			root := d.Create(t)
			d.Submit(t, root, "delegate")
			if !fake.AwaitRequests(3, waitBound) {
				t.Fatal("the parent did not send its ack request")
			}
			fake.Release("child")
			d.WaitIdle(t, root)
			kid := d.Child(t, root, 0)
			d.WaitIdle(t, kid)
			if err := os.WriteFile(filepath.Join(d.Workdir(), ".agents/reader.md"), []byte("---\nname: reader\ndescription: Reads.\n---\n\nThe profile was edited.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			d.Restart(t, false)
			d.Submit(t, kid, "again")
			d.WaitIdle(t, kid)
			var childReqs []harnesstest.Request
			for _, r := range fake.Requests() {
				if r.Messages[0].Parts[0].Text == "child work" {
					childReqs = append(childReqs, r)
				}
			}
			if len(childReqs) != 2 {
				t.Fatalf("child model requests = %d, want 2", len(childReqs))
			}
			for i, r := range childReqs {
				if !strings.Contains(r.System, body) || strings.Contains(r.System, "The profile was edited.") {
					t.Errorf("child request %d system prompt does not hold the declared profile body only:\n%s", i+1, r.System)
				}
			}
			if childReqs[0].System != childReqs[1].System {
				t.Errorf("the system prompt of the child changed after a restart")
			}
		})
	})
}

// rowANewOwnerSendsTheDeclaredSystemPrompt runs a turn on a first owner, edits AGENTS.md, and runs a turn of a second
// owner on the store of the receiver of the first.
func rowANewOwnerSendsTheDeclaredSystemPrompt(t *testing.T) {
	fake := harnesstest.NewChat(t,
		harnesstest.Step{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "1"}},
		harnesstest.Step{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "2"}})
	rcv := newSyncReceiver(t, nil)
	cfg := syncConfig(t, 1, rcv, fake.URL(), nil)
	work := runtimeWorkdir(t, map[string]string{"AGENTS.md": ruleOne})
	r1, err := newWired(t, harness.Options{Store: store.NewMemStore(), Config: cfg, WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	s1, err := r1.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s1, "a", "one")
	if err := r1.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "AGENTS.md"), []byte(ruleTwo), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Sync, cfg.OwnerEpoch = nil, 2
	r2, err := newWired(t, harness.Options{Store: rcv.store, Config: cfg, WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close(context.Background()) })
	s2, err := r2.Open(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s2, "b", "two")
	var systems []string
	for _, raw := range fake.RawRequests() {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(raw, &body) != nil || len(body.Messages) == 0 || body.Messages[0].Role != "system" {
			continue
		}
		b, _ := json.Marshal(body.Messages[0].Content)
		systems = append(systems, string(b))
	}
	if len(systems) != 2 || systems[0] != systems[1] || !strings.Contains(systems[1], "Rule one") || strings.Contains(systems[1], "Rule two") {
		t.Errorf("system prompts = %q, want two equal prompts that hold Rule one", systems)
	}
}

// declaration numbers each declaration blob in order of first sight. The blob holds the work dir of the session in its
// system prompt, so its key and its size differ for each run, and a golden keeps only whether two records name one blob.
func (n *normalizer) declaration(key string) string {
	if a, ok := n.aliases["prefix:"+key]; ok {
		return a
	}
	n.counts["prefix"]++
	a := fmt.Sprintf("prefix#%d", n.counts["prefix"])
	n.aliases["prefix:"+key] = a
	return a
}

// maskDeclaration replaces the blob key and the size of a prefix.declared record in out.
func (n *normalizer) maskDeclaration(in, out map[string]any) {
	if blob, _ := in["blob_key"].(string); strings.HasPrefix(blob, "prefix-") {
		out["blob_key"], out["bytes"] = n.declaration(blob), "<bytes>"
	}
}

// rowACacheBreakpointPastTheLastBlockOfANewDeclarationIsClamped creates a session whose cache breakpoint names the
// instructions block, removes the instructions file, and runs a turn after a compaction between turns.
func rowACacheBreakpointPastTheLastBlockOfANewDeclarationIsClamped(t *testing.T) {
	fake := harnesstest.NewAnthropic(t,
		harnesstest.Step{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "1"}},
		harnesstest.Step{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
		harnesstest.Step{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "2"}},
		harnesstest.Step{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "3"}})
	work := runtimeWorkdir(t, map[string]string{"AGENTS.md": ruleOne})
	r, err := newWired(t, harness.Options{Store: store.NewMemStore(), WorkDir: work, Getenv: func(string) string { return "k" }, Config: config.Config{
		ContextWindowTokens: 100000,
		Providers:           map[string]config.Provider{"anthropic": {BaseURL: fake.URL(), APIKeyEnv: "SKILLS_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	instructions := 2
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "anthropic/claude-sonnet-5-5", SystemCacheBreakpoint: &instructions})
	if err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "a", "one")
	sessionMCPTurn(t, s, "b", "two")
	if err := os.Remove(filepath.Join(work, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	keep := 1
	if _, err := s.Compact(t.Context(), protocol.Compact{KeepTurns: &keep}); err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "c", "three")
	reqs := fake.Requests()
	if len(reqs) != 4 {
		t.Fatalf("model requests = %d, want 4: the turn after the new declaration sent no request", len(reqs))
	}
	last := reqs[len(reqs)-1]
	if strings.Contains(last.System, "Rule one") || len(last.CacheBreakpoints) == 0 || last.CacheBreakpoints[0].Message != -1 {
		t.Errorf("the request after the new declaration holds the removed file, or no system cache marker: %+v\n%s", last.CacheBreakpoints, last.System)
	}
}
