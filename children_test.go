package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// family is a Backend that answers each model call by the last part of its
// history and records each request. A nil answer blocks until the call ends.
type family struct {
	answer func(last eventlog.Part) []eventlog.Message
	mu     sync.Mutex
	reqs   []turn.Request
}

func (f *family) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (f *family) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	m := req.History[len(req.History)-1]
	items := f.answer(m.Parts[len(m.Parts)-1])
	if items == nil {
		<-ctx.Done()
		return turn.Result{}, context.Cause(ctx)
	}
	for _, it := range items {
		if err := out.Item(it); err != nil {
			return turn.Result{}, err
		}
	}
	return turn.Result{}, nil
}

// last returns the last part of the newest request that session sent with
// a last part that starts with prefix, or "".
func (f *family) last(session, prefix string) (turn.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range slices.Backward(f.reqs) {
		m := req.History[len(req.History)-1]
		if p := m.Parts[len(m.Parts)-1].Text; (session == "" || req.SessionID == session) && strings.HasPrefix(p, prefix) {
			return req, p
		}
	}
	return turn.Request{}, ""
}

func task(agent string) eventlog.Message {
	args, _ := json.Marshal(map[string]string{"agent": agent, "prompt": "child work"})
	return eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{
		{Type: eventlog.PartToolCall, CallID: "t1", Name: "task", Arguments: args}}}
}

// delegation answers as the parent and child of the contract scenario. The
// child of agent says "child done", or calls task again with nest, or blocks.
func delegation(agent string, child func() []eventlog.Message) func(eventlog.Part) []eventlog.Message {
	return func(p eventlog.Part) []eventlog.Message {
		switch {
		case p.Text == "delegate":
			return []eventlog.Message{task(agent)}
		case p.Type == eventlog.PartToolResult:
			return []eventlog.Message{say("waiting")}
		case p.Text == "child work":
			return child()
		}
		return []eventlog.Message{say("parent done")}
	}
}

func done() []eventlog.Message { return []eventlog.Message{say("child done")} }

func familyRuntime(t *testing.T, st harness.Store, f *family, own harness.Owner, cfg config.Config, dir string) *harness.Runtime {
	t.Helper()
	r, err := harness.NewWithBackend(harness.Options{Store: st, Owner: own, Config: cfg, WorkDir: dir,
		Tools: []harness.Tool{newProbe("ls", false)}}, f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const report = "A background task you started has finished."

func TestTaskSpawnsAChild(t *testing.T) {
	reader := "---\nname: reader\ndescription: Reads.\ntools: ls\nmodel: test/small\ncolor: blue\n---\n\nOnly read files.\n"
	for _, tc := range []struct {
		name  string
		agent string
		cfg   config.Config
		child func() []eventlog.Message
		kids  int
		check func(t *testing.T, f *family, children []protocol.Session)
	}{
		{name: "the result of the child reaches its parent as an input", agent: "general-purpose", child: done, kids: 1,
			check: func(t *testing.T, f *family, children []protocol.Session) {
				if _, got := f.last("s1", report); !strings.HasSuffix(got, "outcome: done\n\nchild done") {
					t.Errorf("report %q, want outcome done", got)
				}
			}},
		{name: "a profile sets the tools, model, and prompt of the child", agent: "reader", child: done, kids: 1,
			check: func(t *testing.T, f *family, children []protocol.Session) {
				req, _ := f.last(children[0].ID, "child work")
				if len(req.Tools) != 1 || req.Tools[0].Name != "ls" || req.Model != "test/small" || !strings.HasSuffix(req.Instructions, "Only read files.") {
					t.Errorf("child request tools %v, model %s, prompt %q", req.Tools, req.Model, req.Instructions)
				}
			}},
		{name: "an unknown agent spawns no child and names the agents", agent: "nope",
			check: func(t *testing.T, f *family, children []protocol.Session) {
				if _, got := f.last("s1", `task: unknown agent "nope"; the agents are general-purpose, reader`); got == "" {
					t.Error("the task error does not name the agents")
				}
			}},
		{name: "a spawn past max_task_depth is refused", agent: "general-purpose", cfg: config.Config{MaxTaskDepth: 1}, kids: 1,
			child: func() []eventlog.Message { return []eventlog.Message{task("general-purpose")} },
			check: func(t *testing.T, f *family, children []protocol.Session) {
				if _, got := f.last(children[0].ID, "task: max_task_depth 1"); got == "" {
					t.Error("the task call of the child does not fail on max_task_depth")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".agents"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".agents", "reader.md"), []byte(reader), 0o644); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				f := &family{answer: delegation(tc.agent, tc.child)}
				r := familyRuntime(t, harness.NewMemStore(), f, nil, tc.cfg, dir)
				submit(t, create(t, r), text("a", "delegate"))
				if kids := children(t, r); len(kids) != tc.kids {
					t.Errorf("children %+v, want %d", kids, tc.kids)
				} else {
					tc.check(t, f, kids)
				}
				closeRuntime(t, r)
			})
		})
	}
}

func children(t *testing.T, r *harness.Runtime) []protocol.Session {
	t.Helper()
	page, err := r.List(bg, protocol.ListSessions{})
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(page.Sessions, func(s protocol.Session) bool { return s.ParentID != "s1" })
}

func TestOpenSettlesAChildThatCrashed(t *testing.T) {
	dir := t.TempDir()
	eachStore(t, func(t *testing.T, open func() harness.Store) {
		k := killable{make(chan struct{})}
		f1 := &family{answer: delegation("general-purpose", func() []eventlog.Message { return nil })}
		r1 := familyRuntime(t, open(), f1, k, config.Config{}, dir)
		submit(t, create(t, r1), text("a", "delegate"))
		close(k.lost)
		synctest.Wait()
		f2 := &family{answer: f1.answer}
		r2 := familyRuntime(t, open(), f2, nil, config.Config{}, dir)
		if _, err := r2.Open(bg, "s1"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if _, got := f2.last("s1", report); !strings.Contains(got, "outcome: failed: crashed") {
			t.Errorf("report after the restart = %q, want a crashed child", got)
		}
		closeRuntime(t, r1)
		closeRuntime(t, r2)
	})
}
