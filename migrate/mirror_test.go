package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
)

func mirrorRecords(t *testing.T) []json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("testdata/mirror.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var records []json.RawMessage
	for line := range bytes.Lines(data) {
		records = append(records, json.RawMessage(bytes.TrimSpace(line)))
	}
	return records
}

func TestMirrorConvertsEachSession(t *testing.T) {
	ctx := context.Background()
	st := harness.NewMemStore()
	results, err := Mirror(ctx, mirrorRecords(t), st)
	if err != nil || len(results) != 5 || len(Failed(results)) != 0 {
		t.Fatalf("results %+v, %v", results, err)
	}
	p := replay(t, st, "ses_p")
	if got, want := newTranscript(p), []string{"user|text:summary of one", "user|text:two", "assistant|text:done two"}; !slices.Equal(got, want) {
		t.Errorf("parent history %q, want %q", got, want)
	}
	if _, ok := p.Compaction(); !ok {
		t.Error("the parent has no compaction")
	}
	if g, _ := p.Goal(); p.Model() != "anthropic/claude-opus-4-1" || p.Settings().Effort != "high" || g.Condition != "ship it" {
		t.Errorf("parent model %q settings %+v goal %+v", p.Model(), p.Settings(), g)
	}
	if got := p.Children(); !slices.Equal(got, []string{"ses_c", "ses_d", "ses_e", "ses_f"}) {
		t.Errorf("children %v", got)
	}
	if got := p.Unsettled(); !slices.Equal(got, []string{"ses_e"}) {
		t.Errorf("unsettled %v, want only the child that was in a turn", got)
	}
	if q := p.Queue(); len(q) != 1 || q[0].Parts[0].Text != "later" || q[0].Source != "api" || q[0].InputID != "q1" {
		t.Errorf("queue %+v", q)
	}
	if c, _, ok := p.Command("cmd_p1"); !ok || c.Status != "succeeded" || c.Text != "effort is high" {
		t.Errorf("command %+v %v", c, ok)
	}
	c := replay(t, st, "ses_c")
	if got, want := newTranscript(c), []string{"user|text:look", "assistant|text:seen"}; !slices.Equal(got, want) || c.Summary().ParentID != "ses_p" || c.Agent() != "explore" {
		t.Errorf("child history %q parent %q agent %q", got, c.Summary().ParentID, c.Agent())
	}
	ends := map[string]eventlog.TurnEnded{
		"ses_c": {StopReason: eventlog.StopCompleted},
		"ses_d": {StopReason: eventlog.StopInterrupted, Error: string(eventlog.CauseStopped)},
		"ses_e": {StopReason: eventlog.StopFailed, Error: lostToRestart},
		"ses_f": {StopReason: eventlog.StopFailed, Error: "provider down"},
	}
	for id, want := range ends {
		if e := replay(t, st, id).LastEnded(); e.StopReason != want.StopReason || e.Error != want.Error {
			t.Errorf("%s last ended %+v, want %+v", id, e, want)
		}
	}
}

func TestMirrorFailsEachSessionOnASeqGap(t *testing.T) {
	ctx := context.Background()
	records := mirrorRecords(t)
	records = slices.Delete(records, 9, 10)
	st := harness.NewMemStore()
	results, err := Mirror(ctx, records, st)
	if err != nil || len(results) == 0 || len(Failed(results)) != len(results) {
		t.Fatalf("results %+v, %v", results, err)
	}
	for _, r := range results {
		if head, err := st.Head(ctx, r.Session); err != nil || head != 0 {
			t.Errorf("%s has head %d, %v", r.Session, head, err)
		}
	}
}
