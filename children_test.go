package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// usage is the usage of each model call.
	usage eventlog.Usage
	mu    sync.Mutex
	reqs  []turn.Request
}

func (*family) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (f *family) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	m := req.History[len(req.History)-1]
	out.Telemetry(turn.Telemetry{Usage: f.usage})
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
// a last part that starts with prefix, or "". A steer heading does not count, and a task segment answers for a report.
func (f *family) last(session, prefix string) (turn.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range slices.Backward(f.reqs) {
		m := req.History[len(req.History)-1]
		if p := strings.TrimSuffix(strings.TrimPrefix(m.Parts[len(m.Parts)-1].Text, "OPERATOR MESSAGES (address these, then continue the task):\n1. "), "\n"); (session == "" || req.SessionID == session) && (strings.HasPrefix(p, prefix) || prefix == report && strings.HasPrefix(p, "[tasks:")) {
			return req, p
		}
	}
	return turn.Request{}, ""
}

// resulted reports whether a request of session carried a tool result that starts with prefix.
func (f *family) resulted(session, prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.reqs {
		for _, m := range req.History {
			for _, p := range m.Parts {
				if req.SessionID == session && p.Type == eventlog.PartToolResult && strings.HasPrefix(p.Text, prefix) {
					return true
				}
			}
		}
	}
	return false
}

// task calls the task tool n times with agent.
func task(agent string, n int) eventlog.Message {
	args, _ := json.Marshal(map[string]string{"agent": agent, "prompt": "child work"})
	m := eventlog.Message{Role: eventlog.RoleAssistant}
	for i := range n {
		m.Parts = append(m.Parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: fmt.Sprint("t", i), Name: "task", Arguments: args})
	}
	return m
}

// delegation answers as the parent and child of the contract scenario. The
// parent spawns n children of agent, and each child answers with child.
func delegation(agent string, n int, child func() []eventlog.Message) func(eventlog.Part) []eventlog.Message {
	return func(p eventlog.Part) []eventlog.Message {
		switch {
		case p.Text == "delegate":
			return []eventlog.Message{task(agent, n)}
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
	r, err := harness.NewWithBackend(harness.Options{Store: st, Owner: own, Config: cfg, WorkDir: dir}, f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const report = "A background task you started has finished."

const readerProfile = "---\nname: reader\ndescription: Reads.\ntools: ls\nmodel: fake/small\ncolor: blue\n---\n\nOnly read files.\n"

// profileApplied checks that the child ran with the reader profile.
func profileApplied(t *testing.T, f *family, children []protocol.Session) {
	t.Helper()
	req, _ := f.last(children[0].ID, "child work")
	if len(req.Tools) != 1 || req.Tools[0].Name != "ls" || req.Model != "fake/small" || !strings.HasSuffix(req.Instructions, "Only read files.") {
		t.Errorf("child request tools %v, model %s, prompt %q", req.Tools, req.Model, req.Instructions)
	}
}

// rewriteReader returns a Store that rewrites the reader profile of dir
// when the first child.spawned record arrives.
func rewriteReader(t *testing.T, dir string) harness.Store {
	var once sync.Once
	return &hooked{Store: harness.NewMemStore(), before: func(string) {
		once.Do(func() {
			v2 := strings.NewReplacer("tools: ls", "tools: grep", "Only read files.", "Version two.").Replace(readerProfile)
			if err := os.WriteFile(filepath.Join(dir, ".agents", "reader.md"), []byte(v2), 0o644); err != nil {
				t.Error(err)
			}
		})
	}}
}

func TestTaskSpawnsAChild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agent  string
		spawns int
		cfg    config.Config
		// store returns the Store of the runtime, or nil for a MemStore.
		store func(t *testing.T, dir string) harness.Store
		child func() []eventlog.Message
		kids  int
		check func(t *testing.T, f *family, children []protocol.Session)
	}{
		{name: "a profile is read once, when the spawn reads it", agent: "reader", child: done, kids: 1, store: rewriteReader, check: profileApplied},
		{name: "an unknown agent spawns no child and names the agents", agent: "nope",
			check: func(t *testing.T, f *family, children []protocol.Session) {
				if _, got := f.last("s1", `task: unknown agent "nope"; the agents are explore, general-purpose, plan, reader`); got == "" {
					t.Error("the task error does not name the agents")
				}
			}},
		{name: "a spawn past max_task_depth is refused", agent: "general-purpose", cfg: config.Config{MaxTaskDepth: 1}, kids: 1,
			child: func() []eventlog.Message { return []eventlog.Message{task("general-purpose", 1)} },
			check: func(t *testing.T, f *family, children []protocol.Session) {
				if _, got := f.last(children[0].ID, "task: max_task_depth 1"); got == "" {
					t.Error("the task call of the child does not fail on max_task_depth")
				}
			}},
		{name: "a spawn past max_concurrent_tasks is refused", agent: "general-purpose", spawns: 2, cfg: config.Config{MaxConcurrentTasks: 1}, kids: 1,
			child: func() []eventlog.Message { return nil },
			check: func(t *testing.T, f *family, children []protocol.Session) {
				if !f.resulted("s1", "task: max_concurrent_tasks 1") {
					t.Error("the second task call does not fail on max_concurrent_tasks")
				}
			}},
		{name: "an explore child gets only the read-only tools that the runtime has", agent: "explore", child: done, kids: 1, check: readOnly},
		{name: "a plan child gets only the read-only tools that the runtime has", agent: "plan", child: done, kids: 1, check: readOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".agents"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".agents", "reader.md"), []byte(readerProfile), 0o644); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				f := &family{answer: delegation(tc.agent, max(tc.spawns, 1), tc.child)}
				var st harness.Store = harness.NewMemStore()
				if tc.store != nil {
					st = tc.store(t, dir)
				}
				r := familyRuntime(t, st, f, nil, tc.cfg, dir)
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

func readOnly(t *testing.T, f *family, children []protocol.Session) {
	t.Helper()
	req, _ := f.last(children[0].ID, "child work")
	want := []string{"read_file", "glob", "grep", "ls", "session_info"}
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, want) || !slices.Equal(req.AllowedTools, want) {
		t.Errorf("child request tools %v, allowed %v, want %v", names, req.AllowedTools, want)
	}
}

func children(t *testing.T, r *harness.Runtime) []protocol.Session {
	t.Helper()
	return descendants(t, r, "s1")
}

// descendants returns the children of session parent.
func descendants(t *testing.T, r *harness.Runtime, parent string) []protocol.Session {
	t.Helper()
	page, err := r.List(bg, protocol.ListSessions{})
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(page.Sessions, func(s protocol.Session) bool { return s.ParentID != parent })
}

// refusing is a Store that refuses each append to s1 once armed. With arm,
// it also refuses each append to another session, and arms itself.
type refusing struct {
	harness.Store
	arm   bool
	armed atomic.Bool
}

var errRefused = errors.New("append refused")

func (r *refusing) Append(ctx context.Context, id string, expectedSeq uint64, records ...[]byte) error {
	if id != "s1" && r.arm {
		r.armed.Store(true)
		return errRefused
	}
	if id == "s1" && r.armed.Load() {
		return errRefused
	}
	return r.Store.Append(ctx, id, expectedSeq, records...)
}

func TestOpenSettlesEachUnsettledChild(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  bool
		// stop leaves the first runtime before s1 settles its child; free lets the child end.
		stop    func(st *refusing, free func())
		outcome eventlog.Outcome
		report  string
	}{
		{name: "a child that ended before its parent settled it settles done", outcome: eventlog.OutcomeDone, report: "outcome: done\n\nchild done",
			stop: func(st *refusing, free func()) { st.armed.Store(true); free() }},
		{name: "a spawned child with no log settles failed with no report", arm: true, outcome: eventlog.OutcomeFailed,
			stop: func(*refusing, func()) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			eachStore(t, func(t *testing.T, open func() harness.Store) {
				st, release := &refusing{Store: open(), arm: tc.arm}, make(chan struct{})
				free := sync.OnceFunc(func() { close(release) })
				f1 := &family{answer: delegation("general-purpose", 1, func() []eventlog.Message { <-release; return done() })}
				r1 := familyRuntime(t, st, f1, nil, config.Config{}, dir)
				submit(t, create(t, r1), text("a", "delegate"))
				tc.stop(st, free)
				synctest.Wait()
				f2 := &family{answer: f1.answer}
				r2 := familyRuntime(t, open(), f2, nil, config.Config{}, dir)
				if _, err := r2.Open(bg, "s1"); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if got := settled(t, open(), "s1"); got != tc.outcome {
					t.Errorf("child.settled outcome %q, want %q", got, tc.outcome)
				}
				if _, got := f2.last("s1", report); tc.report == "" && got != "" || !strings.Contains(got, tc.report) {
					t.Errorf("report after the restart = %q, want %q", got, tc.report)
				}
				free()
				closeRuntime(t, r1)
				closeRuntime(t, r2)
			})
		})
	}
}

// settled returns the outcome of the first child.settled of session id in st.
func settled(t *testing.T, st harness.Store, id string) eventlog.Outcome {
	t.Helper()
	for _, e := range events(t, st, id) {
		if s, ok := e.(eventlog.ChildSettled); ok {
			return s.Outcome
		}
	}
	return ""
}

// events returns the events of session id in st.
func events(t *testing.T, st harness.Store, id string) []eventlog.Event {
	t.Helper()
	recs, err := st.Read(bg, id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []eventlog.Event
	for _, r := range recs {
		env, err := eventlog.Decode(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, env.Event)
	}
	return out
}

func TestTaskNamesArgumentsOfTheWrongType(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		call := eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{
			{Type: eventlog.PartToolCall, CallID: "t0", Name: "task", Arguments: json.RawMessage(`{"prompt":5}`)}}}
		f := &family{answer: func(p eventlog.Part) []eventlog.Message {
			if p.Type == eventlog.PartToolResult {
				return []eventlog.Message{say("waiting")}
			}
			return []eventlog.Message{call}
		}}
		r := familyRuntime(t, harness.NewMemStore(), f, nil, config.Config{}, t.TempDir())
		submit(t, create(t, r), text("a", "delegate"))
		if _, got := f.last("s1", "task: invalid arguments"); got == "" {
			t.Error("a task call with arguments of the wrong type does not report invalid arguments")
		}
		closeRuntime(t, r)
	})
}

// hooked is a Store that runs before on each append of a child.spawned record
// to session id, and holds the append until before returns.
type hooked struct {
	harness.Store
	before func(id string)
}

func (h *hooked) Append(ctx context.Context, id string, expectedSeq uint64, records ...[]byte) error {
	for _, rec := range records {
		if env, err := eventlog.Decode(rec); err == nil && env.Event.Kind() == (eventlog.ChildSpawned{}).Kind() {
			h.before(id)
		}
	}
	return h.Store.Append(ctx, id, expectedSeq, records...)
}

// gated is a Store that holds each read of session id until open closes.
type gated struct {
	harness.Store
	id   atomic.Value
	open chan struct{}
}

func (g *gated) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
	if id == g.id.Load() {
		select {
		case <-g.open:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.Store.Read(ctx, id, after, limit)
}

func TestAnOpenedParentCountsItsUnsettledChildrenBeforeItRecoversThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, dir := harness.NewMemStore(), t.TempDir()
		cfg := config.Config{MaxConcurrentTasks: 1}
		f1 := &family{answer: delegation("general-purpose", 1, func() []eventlog.Message { return nil })}
		r1 := familyRuntime(t, st, f1, nil, cfg, dir)
		submit(t, create(t, r1), text("a", "delegate"))
		kid := children(t, r1)[0].ID
		closeRuntime(t, r1)
		g := &gated{Store: st, open: make(chan struct{})}
		g.id.Store(kid)
		f2 := &family{answer: f1.answer}
		r2 := familyRuntime(t, g, f2, nil, cfg, dir)
		s, err := r2.Open(bg, "s1")
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		submit(t, s, text("b", "delegate"))
		if _, got := f2.last("s1", "task: max_concurrent_tasks 1"); got == "" {
			t.Error("a spawn beside an unsettled child fails on max_concurrent_tasks only after the recovery reads that child")
		}
		close(g.open)
		closeRuntime(t, r2)
	})
}

func TestTwoSpawnsInOneTreeCountEachOthersChildren(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, calls := make(chan struct{}), atomic.Int32{}
		f := &family{answer: delegation("general-purpose", 1, func() []eventlog.Message {
			if calls.Add(1) > 1 {
				return nil
			}
			<-release
			return []eventlog.Message{task("general-purpose", 1)}
		})}
		open, spawned := make(chan struct{}), 0
		st := &hooked{Store: harness.NewMemStore(), before: func(id string) {
			if id == "s1" {
				if spawned++; spawned == 2 {
					<-open
				}
			}
		}}
		r := familyRuntime(t, st, f, nil, config.Config{MaxConcurrentTasks: 2}, t.TempDir())
		s := create(t, r)
		submit(t, s, text("a", "delegate"))
		first := children(t, r)[0].ID
		submit(t, s, text("b", "delegate"))
		close(release)
		synctest.Wait()
		close(open)
		synctest.Wait()
		if _, got := f.last(first, "task: max_concurrent_tasks 2"); got == "" {
			t.Error("a child spawns beside a spawn of the root that has not settled its append, and the tree passes max_concurrent_tasks")
		}
		closeRuntime(t, r)
	})
}

func TestAgentDefsDirsReplaceTheDefaultProfileDir(t *testing.T) {
	dir := t.TempDir()
	lead := strings.Replace(readerProfile, "name: reader", "name: lead", 1)
	for path, body := range map[string]string{".agents/reader.md": readerProfile, "team/lead.md": lead} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		agent string
		kids  int
		want  string
	}{
		{"reader", 0, `task: unknown agent "reader"; the agents are explore, general-purpose, lead, plan`},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &family{answer: delegation(tc.agent, 1, done)}
				r := familyRuntime(t, harness.NewMemStore(), f, nil, config.Config{AgentDefsDirs: []string{"team"}}, dir)
				submit(t, create(t, r), text("a", "delegate"))
				kids := children(t, r)
				switch {
				case len(kids) != tc.kids:
					t.Errorf("children %+v, want %d", kids, tc.kids)
				case tc.want != "":
					if _, got := f.last("s1", tc.want); got == "" {
						t.Errorf("no task error %q", tc.want)
					}
				default:
					if req, _ := f.last(kids[0].ID, "child work"); !strings.HasSuffix(req.Instructions, "Only read files.") {
						t.Errorf("child prompt %q, want the profile of team/lead.md", req.Instructions)
					}
				}
				closeRuntime(t, r)
			})
		})
	}
}
