package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// scripted returns one scripted stream for each call, and fails the calls
// after the script.
type scripted struct {
	name  string
	turns []func(n int) []provider.Event
	calls int
}

func (p *scripted) Name() string { return p.name }

func (p *scripted) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	if p.calls >= len(p.turns) {
		return nil, io.ErrUnexpectedEOF
	}
	events := p.turns[p.calls](p.calls)
	p.calls++
	return &scriptedStream{events: events}, nil
}

type scriptedStream struct {
	events []provider.Event
	i      int
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.i >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	s.i++
	return s.events[s.i-1], nil
}

func (s *scriptedStream) Close() error { return nil }

func reply(stop provider.StopReason, usage provider.Usage, parts ...message.Part) func(int) []provider.Event {
	return func(n int) []provider.Event {
		m := &message.Message{ID: fmt.Sprintf("msg_rec_%d", n), Role: message.RoleAssistant, Parts: parts}
		return []provider.Event{{Type: provider.EventDone, Message: m, StopReason: stop, Usage: usage}}
	}
}

func say(text string) func(int) []provider.Event {
	return reply(provider.StopEndTurn, provider.Usage{InputTokens: 10, OutputTokens: 5}, &message.Text{Text: text})
}

// A journal that the engine itself writes converts to the transcript that
// the engine shows: a tool call with its arguments, a compaction, and a
// child.
func TestDirConvertsJournalsThatTheEngineRecorded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := &scripted{name: "root", turns: []func(int) []provider.Event{
		say("first answer"),
		say("second answer"),
		reply(provider.StopToolUse, provider.Usage{InputTokens: 20, OutputTokens: 8}, &message.Text{Text: "running"},
			&message.ToolCall{CallID: "tc1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo recorded-output"}`)}),
		say("the command printed it"),
		say("Summary: the user asked twice."),
	}}
	child := &scripted{name: "child", turns: []func(int) []provider.Event{say("child result")}}
	mgr := engine.NewSessionManager(ctx, 0, 0)
	rootSession := mgr.NewRoot(engine.Config{
		Providers:  provider.Registry{"root": root, "child": child},
		Model:      message.ModelRef{Provider: "root", Model: "m1"},
		SessionDir: dir,
		WorkDir:    t.TempDir(),
	})
	for _, text := range []string{"hello", "again", "run echo"} {
		if _, err := rootSession.Prompt(ctx, text); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := rootSession.Compact(ctx, engine.CompactOptions{KeepTurns: 1}); err != nil || res.TurnsFolded == 0 {
		t.Fatalf("compact %+v, %v", res, err)
	}
	childID, err := mgr.Spawn(engine.SpawnOptions{ParentID: rootSession.ID, Prompt: "look", AgentType: "explore",
		Model: message.ModelRef{Provider: "child", Model: "m1"}, ToolNames: []string{"bash"}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the child to finish", func() bool {
		_, info, ok := mgr.SessionAndInfo(childID)
		return ok && info.Status != engine.StatusRunning
	})
	waitFor(t, "the parent to get the report of the child", func() bool {
		h := rootSession.History()
		return h[len(h)-1].Role == message.RoleUser
	})

	st := harness.NewDiskStore(t.TempDir())
	results, err := Dir(ctx, dir, st, message.ModelRef{})
	if err != nil || len(Failed(results)) != 0 {
		t.Fatalf("results %+v, %v", results, err)
	}
	for _, id := range []string{rootSession.ID, childID} {
		s := replay(t, st, id)
		if got, want := newTranscript(s), oldTranscript(t, dir, id); !slices.Equal(got, want) {
			t.Errorf("%s history\n got %q\nwant %q", id, got, want)
		}
	}
	s := replay(t, st, rootSession.ID)
	if !slices.ContainsFunc(newTranscript(s), func(l string) bool { return strings.Contains(l, `{"command":"echo recorded-output"}`) }) {
		t.Errorf("the converted history lost the arguments of the call: %q", newTranscript(s))
	}
	if c, ok := s.Compaction(); !ok || c.Summary == "" {
		t.Errorf("compaction %+v %v", c, ok)
	}
	if !slices.Contains(s.Children(), childID) {
		t.Errorf("children %v lack %s", s.Children(), childID)
	}
	cs := replay(t, st, childID)
	if sum := cs.Summary(); sum.ParentID != rootSession.ID || cs.Agent() != "explore" || !slices.Equal(cs.AllowedTools(), []string{"bash"}) {
		t.Errorf("child summary %+v agent %q tools %v", sum, cs.Agent(), cs.AllowedTools())
	}
}

// waitFor polls cond until it holds, and fails the test when it does not
// hold within ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}
