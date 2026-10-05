package harness_test

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

const (
	freshNote = "the descendant was not actively running, so this was dispatched as a fresh turn with your message; " +
		"check back with task status on this session_id if you want to confirm it actually started"
	queuedNote = "queued for delivery at the descendant's next turn boundary — no need to poll or wait for it, " +
		"unless the descendant's turn is interrupted first by a cancel or an abort " +
		"(an interrupted descendant leaves anything still queued undelivered, like the rest of its own state)"
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
	// first is the arguments of a task call before args, in the same message.
	first map[string]any
	args  map[string]any
	want  string
	// report is in the last report of the child to its parent.
	report string
	// gate holds the first turn of the child until the action has run.
	gate bool
}{
	{name: "status reports a settled child", child: done, args: onChild("status"),
		want: `{"session_id":"KID","parent_id":"s1","depth":1,"status":"done","children":[],"agent_type":"general-purpose","result":"child done","usage":{"input_tokens":5,"output_tokens":1}}`},
	{name: "log returns the transcript of a child", child: done, args: onChild("log"),
		want: `{"session_id":"KID","status":"done","agent_type":"general-purpose","total_messages":2,"returned":2,"entries":[{"role":"user","text":"child work"},{"role":"assistant","text":"child done"}]}`},
	{name: "log keeps the newest entries of a tail", child: done, args: onChild("log", "tail", 1),
		want: `{"session_id":"KID","status":"done","agent_type":"general-purpose","total_messages":2,"returned":1,"entries":[{"role":"assistant","text":"child done"}]}`},
	{name: "cancel stops a running child, which reports canceled", child: block, args: onChild("cancel"),
		want: `{"session_id":"KID","status":"canceled"}`, report: "outcome: canceled"},
	{name: "send runs a settled child again, which reports again", child: done, args: onChild("send", "prompt", "more work"),
		want: `{"session_id":"KID","queued":false,"note":"` + freshNote + `"}`, report: "more done"},
	{name: "send queues for a running child", child: block, args: onChild("send", "prompt", "more work"),
		want: `{"session_id":"KID","queued":true,"note":"` + queuedNote + `"}`},
	{name: "send to a running child reports once the message has run", gate: true, args: onChild("send", "prompt", "more work"),
		want: `{"session_id":"KID","queued":true,"note":"` + queuedNote + `"}`, report: "more done"},
	{name: "cancel withdraws a queued send, so the child stops", child: block, first: onChild("send", "prompt", "more work"),
		args: onChild("cancel"), want: `{"session_id":"KID","status":"canceled"}`, report: "outcome: canceled"},
	{name: "a session that the caller did not spawn is refused", child: done, args: map[string]any{"action": "status", "session_id": "s1"},
		want: "task: s1 is not a session you spawned, directly or transitively"},
	{name: "an unknown session is refused", child: done, args: map[string]any{"action": "log", "session_id": "ses_nope"},
		want: `task: no such session "ses_nope"`},
	{name: "an action needs a session_id", child: done, args: map[string]any{"action": "status"},
		want: `task: session_id is required for action "status"`},
	{name: "a negative tail is refused", child: done, args: onChild("log", "tail", -1),
		want: `task: tail must not be negative for action "log"`},
	{name: "an unknown action names the actions", child: done, args: onChild("stop"),
		want: `task: unknown action "stop" (want one of: spawn, cancel, status, send, log)`},
	{name: "a spawn past max_tree_tokens is refused", child: done, cfg: config.Config{MaxTreeTokens: 10},
		args: map[string]any{"prompt": "child work"}, want: "task: max_tree_tokens 10: this session tree has used N tokens"},
}

// usedTokens matches the token count of a refused spawn. A report that
// reaches the parent in the middle of its turn costs one model call fewer than
// a report that starts a turn, and which one happens is a race.
var usedTokens = regexp.MustCompile(`used \d+ tokens`)

func TestTaskActions(t *testing.T) {
	for _, tc := range taskActionRows {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				kid := ""
				f := &family{usage: eventlog.Usage{InputTokens: 5, OutputTokens: 1}}
				release := make(chan struct{})
				free := sync.OnceFunc(func() { close(release) })
				defer free()
				child := tc.child
				if tc.gate {
					child = func() []eventlog.Message { <-release; return done() }
				}
				spawn := delegation("general-purpose", 1, child)
				f.answer = func(p eventlog.Part) []eventlog.Message {
					switch p.Text {
					case "act":
						var all []map[string]any
						for _, in := range []map[string]any{tc.first, tc.args} {
							args := map[string]any{}
							for k, v := range in {
								if v == "KID" {
									v = kid
								}
								args[k] = v
							}
							if in != nil {
								all = append(all, args)
							}
						}
						return []eventlog.Message{calls("task", all...)}
					case "more work":
						return []eventlog.Message{say("more done")}
					}
					return spawn(p)
				}
				st := harness.NewMemStore()
				r := familyRuntime(t, st, f, nil, tc.cfg, t.TempDir())
				s := create(t, r)
				submit(t, s, text("a", "delegate"))
				kid = children(t, r)[0].ID
				submit(t, s, text("b", "act"))
				free()
				synctest.Wait()
				res := f.results("s1")
				if got := usedTokens.ReplaceAllString(strings.ReplaceAll(res[len(res)-1], kid, "KID"), "used N tokens"); got != tc.want {
					t.Errorf("result\n%s\nwant\n%s", got, tc.want)
				}
				if _, got := f.last("s1", report); !strings.Contains(got, tc.report) {
					t.Errorf("last report %q, want %q", got, tc.report)
				}
				for _, e := range events(t, st, "s1") {
					if c, ok := e.(eventlog.ChildSpawned); ok && c.Agent != "general-purpose" {
						t.Errorf("child.spawned %+v, want agent general-purpose", c)
					}
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

func TestTaskTreeReachesAGrandchild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grand func() []eventlog.Message
		// act runs on s1 after the delegation; KID and GRAND name the descendants.
		act  func(s *harness.Session) error
		args map[string]any
		// want is a prefix of the last tool result of s1; empty: the grandchild settles canceled in silence.
		want string
	}{
		{name: "a tree interrupt stops the grandchild, and its parent starts no turn", grand: block,
			act: func(s *harness.Session) error { return s.Interrupt(bg, protocol.Interrupt{Tree: true}) }},
		{name: "cancel of a child stops the grandchild, and the child starts no turn", grand: block, args: onChild("cancel")},
		{name: "status reaches a grandchild", grand: done, args: map[string]any{"action": "status", "session_id": "GRAND"},
			want: `{"session_id":"GRAND","parent_id":"KID","depth":2,"status":"done"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var kid, grand string
				f := &family{answer: generations(tc.grand, func() eventlog.Message {
					args := map[string]any{}
					for k, v := range tc.args {
						args[k] = strings.NewReplacer("GRAND", grand, "KID", kid).Replace(fmt.Sprint(v))
					}
					return calls("task", args)
				})}
				st := harness.NewMemStore()
				r := familyRuntime(t, st, f, nil, config.Config{}, t.TempDir())
				s := create(t, r)
				submit(t, s, text("a", "delegate"))
				kid = children(t, r)[0].ID
				grand = descendants(t, r, kid)[0].ID
				if tc.act != nil {
					if err := tc.act(s); err != nil {
						t.Fatal(err)
					}
				} else {
					submit(t, s, text("b", "act"))
				}
				synctest.Wait()
				if tc.want != "" {
					res := f.results("s1")
					if got := strings.NewReplacer(grand, "GRAND", kid, "KID").Replace(res[len(res)-1]); !strings.HasPrefix(got, tc.want) {
						t.Errorf("result\n%s\nwant prefix\n%s", got, tc.want)
					}
				} else if got := settled(t, st, kid); got != eventlog.OutcomeCanceled {
					t.Errorf("child.settled of the grandchild %q, want canceled", got)
				}
				if _, got := f.last(kid, report); tc.want == "" && got != "" {
					t.Errorf("the child inside the stopped tree got the report %q", got)
				}
				closeRuntime(t, r)
			})
		})
	}
}

func TestInterruptTreeStopsEveryDescendant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := harness.NewMemStore()
		f := &family{answer: delegation("general-purpose", 1, block)}
		r := familyRuntime(t, st, f, nil, config.Config{}, t.TempDir())
		s := create(t, r)
		submit(t, s, text("a", "delegate"))
		if err := s.Interrupt(bg, protocol.Interrupt{Tree: true}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := settled(t, st, "s1"); got != eventlog.OutcomeCanceled {
			t.Errorf("child.settled outcome %q, want canceled", got)
		}
		if _, got := f.last("s1", report); got != "" {
			t.Errorf("the parent inside the interrupted tree got the report %q", got)
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
