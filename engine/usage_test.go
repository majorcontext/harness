package engine

import (
	"context"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestListSessionsCarriesUsage is the red-first test for SessionInfo's
// cheap, cumulative-usage summary (issue #62 layer 2): GET /session/status
// must be able to surface usage for a non-resident session without paying
// for a full LoadSession (which replays and reconstructs canonical
// messages) — ListSessions already scans each log's headers for Messages;
// this extends that same cheap scan to sum each record's optional Usage.
func TestListSessionsCarriesUsage(t *testing.T) {
	turn1 := asstTurn(provider.StopToolUse, &message.Text{Text: "running"}, toolCall("tc1", "bash", `{"command":"echo hi"}`))
	turn1[len(turn1)-1].Usage = provider.Usage{InputTokens: 10, OutputTokens: 20}
	turn2 := asstTurn(provider.StopEndTurn, &message.Text{Text: "done"})
	turn2[len(turn2)-1].Usage = provider.Usage{InputTokens: 150, OutputTokens: 5}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{turn1, turn2}}

	dir := t.TempDir()
	s := NewSession(persistCfg(dir, prov))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	infos, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("ListSessions = %d entries, want 1", len(infos))
	}
	info := infos[0]
	want := provider.Usage{InputTokens: 160, OutputTokens: 25}
	if info.Usage != want {
		t.Errorf("SessionInfo.Usage = %+v, want %+v", info.Usage, want)
	}
	if info.LastInputTokens != 150 {
		t.Errorf("SessionInfo.LastInputTokens = %d, want 150 (the most recent turn's input tokens)", info.LastInputTokens)
	}
}

// TestLastUsageReflectsMostRecentTurn is the red-first test for
// Session.LastUsage: the input-token count of the LAST model response,
// distinct from cumulative Usage() — orchestrators watching GET /session for
// a rotation signal want "how big did the request I just sent get", not
// only the running total (issue #62 layer 2).
func TestLastUsageReflectsMostRecentTurn(t *testing.T) {
	turn1 := asstTurn(provider.StopToolUse, &message.Text{Text: "running"}, toolCall("tc1", "bash", `{"command":"echo hi"}`))
	turn1[len(turn1)-1].Usage = provider.Usage{InputTokens: 10, OutputTokens: 20}
	turn2 := asstTurn(provider.StopEndTurn, &message.Text{Text: "done"})
	turn2[len(turn2)-1].Usage = provider.Usage{InputTokens: 150, OutputTokens: 5}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{turn1, turn2}}

	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	if _, ok := s.LastUsage(); ok {
		t.Error("LastUsage ok before any turn, want false")
	}

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	last, ok := s.LastUsage()
	if !ok {
		t.Fatal("LastUsage not ok after a completed turn")
	}
	if last != (provider.Usage{InputTokens: 150, OutputTokens: 5}) {
		t.Errorf("LastUsage = %+v, want the most recent turn's usage only", last)
	}
}

// TestUsageSurvivesReload is the red-first test for issue #62 layer 2's
// reload correctness gap: cumulative Usage() and LastUsage() must survive a
// process restart (LoadSession), or an orchestrator polling a freshly
// reloaded session sees a false "zero usage" and never rotates it — exactly
// the scenario the reported incident's re-armed goal hit.
func TestUsageSurvivesReload(t *testing.T) {
	turn1 := asstTurn(provider.StopToolUse, &message.Text{Text: "running"}, toolCall("tc1", "bash", `{"command":"echo hi"}`))
	turn1[len(turn1)-1].Usage = provider.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 4}
	turn2 := asstTurn(provider.StopEndTurn, &message.Text{Text: "done"})
	turn2[len(turn2)-1].Usage = provider.Usage{InputTokens: 100, OutputTokens: 200, CacheReadTokens: 30, CacheWriteTokens: 40}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{turn1, turn2}}

	dir := t.TempDir()
	cfg := Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	want := provider.Usage{InputTokens: 110, OutputTokens: 220, CacheReadTokens: 33, CacheWriteTokens: 44}
	if got := s.Usage(); got != want {
		t.Fatalf("Usage before reload = %+v, want %+v", got, want)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Usage(); got != want {
		t.Errorf("Usage after reload = %+v, want %+v (cumulative usage lost across reload)", got, want)
	}
	last, ok := loaded.LastUsage()
	if !ok {
		t.Fatal("LastUsage after reload not ok")
	}
	if last != turn2[len(turn2)-1].Usage {
		t.Errorf("LastUsage after reload = %+v, want %+v", last, turn2[len(turn2)-1].Usage)
	}
}

// TestLastActivityAtTracksMostRecentMessage is the red-first test for
// Session.LastActivityAt (issue #62 layer 3, the liveness field): it must
// reflect the most recently appended message, so a fleet monitor can read a
// single absolute timestamp instead of double-sampling Seq deltas to infer
// whether a session is quietly working or wedged.
func TestLastActivityAtTracksMostRecentMessage(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	before := s.LastActivityAt()
	if before != s.CreatedAt() {
		t.Errorf("LastActivityAt before any message = %v, want CreatedAt %v", before, s.CreatedAt())
	}

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	history := s.History()
	want := history[len(history)-1].CreatedAt
	if got := s.LastActivityAt(); !got.Equal(want) {
		t.Errorf("LastActivityAt = %v, want the last message's CreatedAt %v", got, want)
	}
}
