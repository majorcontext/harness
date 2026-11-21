package harness_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func TestTurnContextReachesEachModelCallOfOneTurnOnly(t *testing.T) {
	const sentinel = "ctx-sentinel"
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{},
		callStep("echo"),
		harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("echo"), Reply: harnesstest.Reply{Text: "done"}},
		harnesstest.Step{Name: "again", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}})
	r, _, _ := codexRuntime(t, s, false, false, "", newProbe("echo", false))
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []struct {
		id, text string
		opts     []harness.PromptOption
	}{{"a", "run", []harness.PromptOption{harness.WithTurnContext(sentinel)}}, {"b", "again", nil}} {
		turn, err := sess.Prompt(bg, protocol.Input{ID: in.id, Parts: []protocol.Part{{Type: protocol.PartText, Text: in.text}}}, in.opts...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := turn.Wait(bg); err != nil {
			t.Fatal(err)
		}
	}
	reqs := s.Requests()
	if len(reqs) != 3 {
		t.Fatalf("model requests = %d, want 3", len(reqs))
	}
	want := "user text :<harness-engine-context>\n" + sentinel + "\n</harness-engine-context>"
	for i, req := range reqs[:2] {
		got := transcript(req)
		at := -1
		for j, line := range got {
			if line == "user text :run" {
				at = j + 1
			}
		}
		if at < 0 || at >= len(got) || got[at] != want {
			t.Errorf("request %d = %q, want the context right after the input", i, got)
		}
	}
	if got := strings.Join(transcript(reqs[2]), "\n"); strings.Contains(got, sentinel) {
		t.Errorf("request of turn 2 = %q, want no turn context", got)
	}
	var stored strings.Builder
	head := sess.Snapshot().HeadSeq
	for e, err := range sess.Events(bg, 0) {
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(e)
		stored.Write(b)
		if e.Seq >= head {
			break
		}
	}
	page, err := sess.Messages(bg, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(page)
	stored.Write(b)
	if strings.Contains(stored.String(), sentinel) {
		t.Error("the events or the history hold the turn context")
	}
}
