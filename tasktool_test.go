package harness_test

import (
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
)

// onChild returns the arguments of action on the child, with more as key, value pairs.
func onChild(action string, more ...any) map[string]any {
	args := map[string]any{"action": action, "session_id": "KID"}
	for i := 0; i < len(more); i += 2 {
		args[more[i].(string)] = more[i+1]
	}
	return args
}

var taskActionRows = []struct {
	name  string
	child func() []eventlog.Message
	cfg   config.Config
	args  map[string]any
	want  string
}{
	{name: "a spawn past max_tree_tokens is refused", child: done, cfg: config.Config{MaxTreeTokens: 10},
		args: map[string]any{"prompt": "child work"}, want: "task: engine: task tree token budget exceeded"},
}

func TestTaskActions(t *testing.T) {
	for _, tc := range taskActionRows {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				kid := ""
				f := &family{usage: eventlog.Usage{InputTokens: 5, OutputTokens: 1}}
				spawn := delegation("general-purpose", 1, tc.child)
				f.answer = func(p eventlog.Part) []eventlog.Message {
					if p.Text == "act" {
						return []eventlog.Message{calls("task", tc.args)}
					}
					return spawn(p)
				}
				st := harness.NewMemStore()
				r := familyRuntime(t, st, f, nil, tc.cfg, t.TempDir())
				s := create(t, r)
				submit(t, s, text("a", "delegate"))
				kid = children(t, r)[0].ID
				submit(t, s, text("b", "act"))
				synctest.Wait()
				res := f.results("s1")
				if got := strings.ReplaceAll(res[len(res)-1], kid, "KID"); got != tc.want {
					t.Errorf("result\n%s\nwant\n%s", got, tc.want)
				}
				closeRuntime(t, r)
			})
		})
	}
}

func block() []eventlog.Message { return nil }

// generations answers as a root that spawns a child, which spawns a
// grandchild that answers with grand. act is the answer of the root to "act".
func generations(grand func() []eventlog.Message, act func() eventlog.Message) func(eventlog.Part) []eventlog.Message {
	return func(p eventlog.Part) []eventlog.Message {
		switch p.Text {
		case "act":
			return []eventlog.Message{act()}
		case "child work":
			return []eventlog.Message{calls("task", map[string]any{"prompt": "grand work"})}
		case "grand work":
			return grand()
		}
		return delegation("general-purpose", 1, nil)(p)
	}
}

func TestCancelOfAChildStopsTheGrandchildAndTheChildStartsNoTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var kid string
		f := &family{answer: generations(block, func() eventlog.Message {
			return calls("task", onChild("cancel", "session_id", kid))
		})}
		st := harness.NewMemStore()
		r := familyRuntime(t, st, f, nil, config.Config{}, t.TempDir())
		s := create(t, r)
		submit(t, s, text("a", "delegate"))
		kid = children(t, r)[0].ID
		submit(t, s, text("b", "act"))
		synctest.Wait()
		if got := settled(t, st, kid); got != eventlog.OutcomeCanceled {
			t.Errorf("child.settled of the grandchild %q, want canceled", got)
		}
		if _, got := f.last(kid, report); got != "" {
			t.Errorf("the child inside the stopped tree got the report %q", got)
		}
		closeRuntime(t, r)
	})
}

func TestTaskStatusReachesADescendantBelowALoweredMaxTaskDepth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var great string
		answer := func(p eventlog.Part) []eventlog.Message {
			switch p.Text {
			case "act":
				return []eventlog.Message{calls("task", map[string]any{"action": "status", "session_id": great})}
			case "grand work":
				return []eventlog.Message{calls("task", map[string]any{"prompt": "great work"})}
			case "great work":
				return done()
			}
			return generations(done, nil)(p)
		}
		st, dir := harness.NewMemStore(), t.TempDir()
		r1 := familyRuntime(t, st, &family{answer: answer}, nil, config.Config{}, dir)
		submit(t, create(t, r1), text("a", "delegate"))
		kid := children(t, r1)[0].ID
		grand := descendants(t, r1, kid)[0].ID
		great = descendants(t, r1, grand)[0].ID
		closeRuntime(t, r1)
		f2 := &family{answer: answer}
		r2 := familyRuntime(t, st, f2, nil, config.Config{MaxTaskDepth: 1}, dir)
		s, err := r2.Open(bg, "s1")
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		submit(t, s, text("b", "act"))
		synctest.Wait()
		res := f2.results("s1")
		if want := `{"session_id":"` + great + `","parent_id":"` + grand + `","depth":3`; len(res) == 0 || !strings.HasPrefix(res[len(res)-1], want) {
			t.Errorf("status of a great-grandchild after max_task_depth drops to 1: %v, want prefix %s", res, want)
		}
		closeRuntime(t, r2)
	})
}
