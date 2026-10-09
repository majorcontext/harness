package harness_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
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

// stoppingSync holds every batch of a session other than s1 until release
// closes, then rejects the first batch of stop for good and acknowledges the rest.
type stoppingSync struct {
	release chan struct{}
	mu      sync.Mutex
	stop    string
	spent   bool
}

func (y *stoppingSync) Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error) {
	if b.Session != "s1" {
		select {
		case <-y.release:
		case <-ctx.Done():
			return protocol.SyncAck{}, ctx.Err()
		}
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	if b.Session == y.stop && !y.spent {
		y.spent = true
		return protocol.SyncAck{}, harness.ErrSyncRejected
	}
	return protocol.SyncAck{Head: b.FromSeq + uint64(len(b.Records)) - 1}, nil
}

// parkingStore parks the first read of session park once the turn of session
// ender ends, until resume closes.
type parkingStore struct {
	harness.Store
	ender, park string
	armed       chan struct{}
	entered     chan struct{}
	resume      chan struct{}
	parked      atomic.Bool
}

func (s *parkingStore) Append(ctx context.Context, id string, expectedSeq uint64, records ...[]byte) error {
	if id == s.ender {
		for _, r := range records {
			if env, err := eventlog.Decode(r); err == nil {
				if _, ok := env.Event.(eventlog.TurnEnded); ok {
					close(s.armed)
				}
			}
		}
	}
	return s.Store.Append(ctx, id, expectedSeq, records...)
}

func (s *parkingStore) Head(ctx context.Context, id string) (uint64, error) {
	if id == s.park {
		select {
		case <-s.armed:
			if s.parked.CompareAndSwap(false, true) {
				close(s.entered)
				select {
				case <-s.resume:
				case <-ctx.Done():
				}
			}
		default:
		}
	}
	return s.Store.Head(ctx, id)
}

func TestCancelOfAGrandchildWhoseParentStoppedReportsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var grand string
		f := &family{answer: generations(block, func() eventlog.Message {
			return calls("task", onChild("cancel", "session_id", grand))
		})}
		y := &stoppingSync{release: make(chan struct{})}
		ps := &parkingStore{Store: harness.NewMemStore(), armed: make(chan struct{}), entered: make(chan struct{}), resume: make(chan struct{})}
		r, err := harness.NewWithBackend(harness.Options{Store: ps, Sync: y, WorkDir: t.TempDir()}, f)
		if err != nil {
			t.Fatal(err)
		}
		s := create(t, r)
		submit(t, s, text("a", "delegate"))
		kid := children(t, r)[0].ID
		grand = descendants(t, r, kid)[0].ID
		y.stop, ps.ender, ps.park = kid, grand, kid
		reportInputs := func() (n int) {
			for _, id := range []string{"s1", kid, grand} {
				for _, e := range events(t, ps, id) {
					if in, ok := e.(eventlog.InputAdmitted); ok && in.Source == "child" {
						n++
					}
				}
			}
			return n
		}
		before := reportInputs()
		close(y.release)
		synctest.Wait()
		submit(t, s, text("b", "act"))
		<-ps.entered
		if _, err := r.Open(bg, kid); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		close(ps.resume)
		synctest.Wait()
		if got := reportInputs() - before; got != 1 {
			t.Errorf("report inputs the cancel added to the tree = %d, want 1", got)
		}
		closeRuntime(t, r)
	})
}
