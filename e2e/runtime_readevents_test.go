package e2e

import (
	"context"
	"reflect"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func collectEvents(t *testing.T, seq func(func(protocol.Event, error) bool)) []protocol.Event {
	t.Helper()
	var out []protocol.Event
	for e, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// ReadEvents yields the events after a cursor the way a view of the same log
// does, reads nothing at the head or for an unknown session, and appends nothing.
func TestReadEventsEqualsAViewOfTheSameLog(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	stores := map[string]func(t *testing.T) harness.Store{
		"mem":  func(*testing.T) harness.Store { return harness.NewMemStore() },
		"disk": func(t *testing.T) harness.Store { return harness.NewDiskStore(t.TempDir()) },
	}
	for name, newStore := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := newStore(t)
			fake := harnesstest.NewChat(t,
				harnesstest.Step{Name: "bash", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash", Input: ftArgs("command", "echo hi")}}}},
				harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "ok"}},
				harnesstest.Step{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "second"}})
			r, err := harness.New(harness.Options{Store: st, WorkDir: t.TempDir(), Config: config.Config{ContextWindowTokens: 100000,
				Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_KEY"}}}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.Create(ctx, protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
			if err != nil {
				t.Fatal(err)
			}
			for turn, text := range []string{"go", "two"} {
				if _, err := s.Submit(ctx, protocol.Input{ID: text, Parts: []protocol.Part{{Type: protocol.PartText, Text: text}}}); err != nil {
					t.Fatal(err)
				}
				ended := 0
				for e, err := range s.Events(ctx, 0) {
					if err != nil {
						t.Fatal(err)
					}
					if e.Kind == "turn.ended" {
						ended++
					}
					if ended == turn+1 {
						break
					}
				}
			}
			if err := r.Close(ctx); err != nil {
				t.Fatal(err)
			}

			head, err := st.Head(ctx, "s1")
			if err != nil {
				t.Fatal(err)
			}
			v, err := harness.OpenView(ctx, st, "s1")
			if err != nil {
				t.Fatal(err)
			}
			if head < 8 {
				t.Fatalf("head = %d, want a log with two turns and a tool call", head)
			}
			for k := uint64(0); k <= head+1; k++ {
				got := collectEvents(t, harness.ReadEvents(ctx, st, "s1", k))
				want := collectEvents(t, v.Events(ctx, k))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("ReadEvents(%d) = %v, want %v", k, got, want)
				}
				if wantLen := max(int(head)-int(k), 0); len(got) != wantLen {
					t.Fatalf("ReadEvents(%d) yielded %d events, want %d", k, len(got), wantLen)
				}
			}
			if got := collectEvents(t, harness.ReadEvents(ctx, st, "s1", head)); len(got) != 0 {
				t.Errorf("ReadEvents at the head = %v, want nothing", got)
			}
			if got := collectEvents(t, harness.ReadEvents(ctx, st, "unknown", 0)); len(got) != 0 {
				t.Errorf("ReadEvents of an unknown session = %v, want nothing", got)
			}
			if after, err := st.Head(ctx, "s1"); err != nil || after != head {
				t.Errorf("head after ReadEvents = %d, %v, want %d", after, err, head)
			}
			if after, err := st.Head(ctx, "unknown"); err != nil || after != 0 {
				t.Errorf("head of the unknown session after ReadEvents = %d, %v, want 0", after, err)
			}
		})
	}
}
