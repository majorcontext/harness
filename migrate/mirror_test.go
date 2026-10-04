package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
)

func TestMirrorConvertsEachSession(t *testing.T) {
	ctx := context.Background()
	data, err := os.ReadFile("testdata/mirror.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var records []json.RawMessage
	for line := range bytes.Lines(data) {
		records = append(records, json.RawMessage(bytes.TrimSpace(line)))
	}
	st := harness.NewMemStore()
	results, err := Mirror(ctx, records, st)
	if err != nil || len(results) != 2 || len(Failed(results)) != 0 {
		t.Fatalf("results %+v, %v", results, err)
	}
	p := replay(t, st, "ses_p")
	if got, want := newTranscript(p), []string{"user|text:summary of one", "user|text:two", "assistant|text:done two"}; !slices.Equal(got, want) {
		t.Errorf("parent history %q, want %q", got, want)
	}
	if _, ok := p.Compaction(); !ok {
		t.Error("the parent has no compaction")
	}
	if g, _ := p.Goal(); p.Model() != "anthropic/claude-opus-4-1" || p.Settings().Effort != "high" || g.Condition != "ship it" || !slices.Equal(p.Unsettled(), []string{"ses_c"}) {
		t.Errorf("parent model %q settings %+v goal %+v children %v", p.Model(), p.Settings(), g, p.Unsettled())
	}
	c := replay(t, st, "ses_c")
	if got, want := newTranscript(c), []string{"user|text:look", "assistant|text:seen"}; !slices.Equal(got, want) || c.Summary().ParentID != "ses_p" || c.Agent() != "explore" {
		t.Errorf("child history %q parent %q agent %q", got, c.Summary().ParentID, c.Agent())
	}
}
