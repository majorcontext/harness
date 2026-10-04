package harness_test

import (
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

const freshNote = "the descendant was not actively running, so this was dispatched as a fresh turn with your message; " +
	"check back with task status on this session_id if you want to confirm it actually started"

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
		want: `{"session_id":"KID","queued":true,"note":"queued for delivery at the descendant's next turn boundary — no need to poll or wait for it"}`},
	{name: "send to a running child reports once the message has run", gate: true, args: onChild("send", "prompt", "more work"),
		want: `{"session_id":"KID","queued":true,"note":"queued for delivery at the descendant's next turn boundary — no need to poll or wait for it"}`, report: "more done"},
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
		args: map[string]any{"prompt": "child work"}, want: "task: max_tree_tokens 10: this session tree has used 24 tokens"},
}

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
						args := map[string]any{}
						for k, v := range tc.args {
							if v == "KID" {
								v = kid
							}
							args[k] = v
						}
						return []eventlog.Message{calls("task", args)}
					case "more work":
						return []eventlog.Message{say("more done")}
					}
					return spawn(p)
				}
				r := familyRuntime(t, harness.NewMemStore(), f, nil, tc.cfg, t.TempDir())
				s := create(t, r)
				submit(t, s, text("a", "delegate"))
				kid = children(t, r)[0].ID
				submit(t, s, text("b", "act"))
				free()
				synctest.Wait()
				res := f.results("s1")
				if got := strings.ReplaceAll(res[len(res)-1], kid, "KID"); got != tc.want {
					t.Errorf("result\n%s\nwant\n%s", got, tc.want)
				}
				if _, got := f.last("s1", report); !strings.Contains(got, tc.report) {
					t.Errorf("last report %q, want %q", got, tc.report)
				}
				closeRuntime(t, r)
			})
		})
	}
}

func block() []eventlog.Message { return nil }

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
		if got := settled(t, st); got != eventlog.OutcomeCanceled {
			t.Errorf("child.settled outcome %q, want canceled", got)
		}
		if _, got := f.last("s1", report); got != "" {
			t.Errorf("the parent inside the interrupted tree got the report %q", got)
		}
		closeRuntime(t, r)
	})
}
