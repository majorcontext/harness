package harness_test

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/turn"
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

func TestTurnContextKeepsItsPlaceWhenANoticeChangesInTheTurn(t *testing.T) {
	weather := harnesstest.NewMCPServer(t, mcpWeatherSpec())
	weather.FailInitialize(1)
	synctest.Test(t, func(t *testing.T) {
		f := mcpActions()
		r := mcpRuntime(t, f, map[string]*harnesstest.MCPServer{"weather": weather})
		s := create(t, r)
		if _, err := s.Prompt(bg, text("a", "connect"), harness.WithTurnContext("ctx-sentinel")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		f.mu.Lock()
		reqs := slices.Clone(f.reqs)
		f.mu.Unlock()
		if len(reqs) != 2 {
			t.Fatalf("model requests = %d, want 2", len(reqs))
		}
		place := func(req turn.Request) (int, int) {
			i := slices.IndexFunc(req.Pins, func(p turn.Pin) bool { return p.Kind == turn.TurnContextKind })
			return i, req.Pins[i].At
		}
		i1, at1 := place(reqs[0])
		i2, at2 := place(reqs[1])
		if i1 != i2 || at1 != at2 || !slices.IsSortedFunc(reqs[1].Pins, func(a, b turn.Pin) int { return cmp.Compare(a.At, b.At) }) {
			t.Errorf("pins of request 2 = %+v, want the turn context at index %d, place %d, and every pin in place order", reqs[1].Pins, i1, at1)
		}
		closeRuntime(t, r)
	})
}
