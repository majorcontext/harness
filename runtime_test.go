package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

var bg = context.Background()

const (
	cutOff      = "cut off before a result was recorded; check whether it took effect before running it again"
	interrupted = "interrupted before a result was recorded; check whether it took effect before running it again"
)

// fake is a scripted Backend. Each Run sends itself on runs; the test sends
// the items of the turn on items and closes items to end the turn. When the
// turn's ctx ends, it reports late, then waits for stuck when it is set.
type fake struct {
	steering bool
	deaf     bool
	late     []eventlog.Message
	stuck    chan struct{}
	runs     chan fakeRun
}

type fakeRun struct {
	req   turn.Request
	items chan<- eventlog.Message
}

func newFake() *fake { return &fake{runs: make(chan fakeRun)} }

func (f *fake) Capabilities(string) turn.Capabilities { return turn.Capabilities{Steering: f.steering} }

func (f *fake) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	items := make(chan eventlog.Message)
	select {
	case f.runs <- fakeRun{req, items}:
	case <-ctx.Done():
		return turn.Result{}, context.Cause(ctx)
	}
	for {
		select {
		case m, ok := <-items:
			if !ok {
				return turn.Result{Usage: eventlog.Usage{InputTokens: 3, OutputTokens: 1}}, nil
			}
			if err := f.report(out, m); err != nil {
				return turn.Result{}, err
			}
		case <-ctx.Done():
			return f.stop(ctx, out)
		}
	}
}

// report reports m, then an answer to each steer input that it takes.
func (f *fake) report(out turn.Sink, m eventlog.Message) error {
	if err := out.Item(m); err != nil || !f.steering || f.deaf {
		return err
	}
	in, err := out.Steer()
	for _, s := range in {
		if err == nil {
			err = f.report(out, say("steered: "+s.Parts[0].Text))
		}
	}
	return err
}

func (f *fake) stop(ctx context.Context, out turn.Sink) (turn.Result, error) {
	for _, m := range f.late {
		if out.Item(m) != nil {
			break
		}
	}
	if f.stuck != nil {
		<-f.stuck
	}
	return turn.Result{}, context.Cause(ctx)
}

func (r fakeRun) emit(m eventlog.Message) {
	r.items <- m
	synctest.Wait()
}

func (r fakeRun) end() {
	close(r.items)
	synctest.Wait()
}

func say(text string) eventlog.Message {
	return eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
}

func callTool(id string) eventlog.Message {
	return eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{
		{Type: eventlog.PartToolCall, CallID: id, Name: "bash", Arguments: json.RawMessage(`{}`)}}}
}

func toolResult(id string) eventlog.Message {
	return eventlog.Message{Role: eventlog.RoleTool, Parts: []eventlog.Part{{Type: eventlog.PartToolResult, CallID: id, Text: "ok"}}}
}

func text(id, s string) protocol.Input {
	return protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: s}}}
}

// eachStore runs body in a synctest bubble once per Store. open returns the
// store as a restarted process sees it: the same MemStore, or a new
// DiskStore on the same directory.
func eachStore(t *testing.T, body func(t *testing.T, open func() harness.Store)) {
	t.Helper()
	mem := harness.NewMemStore()
	stores := []struct {
		name string
		open func(dir string) harness.Store
	}{
		{"mem", func(string) harness.Store { return mem }},
		{"disk", func(dir string) harness.Store { return harness.NewDiskStore(dir) }},
	}
	for _, s := range stores {
		t.Run(s.name, func(t *testing.T) {
			dir := t.TempDir()
			synctest.Test(t, func(t *testing.T) { body(t, func() harness.Store { return s.open(dir) }) })
		})
	}
}

func runtime(t *testing.T, st harness.Store, b turn.Backend) *harness.Runtime {
	t.Helper()
	r, err := harness.NewWithBackend(harness.Options{Store: st}, b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func create(t *testing.T, r *harness.Runtime) *harness.Session {
	t.Helper()
	s, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "test/model", Origin: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func submit(t *testing.T, s *harness.Session, in protocol.Input) protocol.Admitted {
	t.Helper()
	a, err := s.Submit(bg, in)
	if err != nil {
		t.Fatalf("Submit(%s): %v", in.ID, err)
	}
	synctest.Wait()
	return a
}

func closeRuntime(t *testing.T, r *harness.Runtime) {
	t.Helper()
	if err := r.Close(bg); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// logOf renders each record of session s1 in st after seq as one line.
func logOf(t *testing.T, st harness.Store, after uint64) []string {
	t.Helper()
	recs, err := st.Read(bg, "s1", after, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		var env struct {
			K string          `json:"k"`
			D json.RawMessage `json:"d"`
		}
		if err := json.Unmarshal(r.Data, &env); err != nil {
			t.Fatal(err)
		}
		out = append(out, line(env.K, env.D))
	}
	return out
}

func line(kind string, d json.RawMessage) string {
	var p struct {
		Epoch      uint64
		InputID    string   `json:"input_id"`
		InputIDs   []string `json:"input_ids"`
		StopReason string   `json:"stop_reason"`
		Error      string
		Cause      string
		Count      int
		Message    eventlog.Message
	}
	_ = json.Unmarshal(d, &p)
	f := []string{kind}
	for _, s := range append([]string{p.InputID, p.StopReason, p.Error, p.Cause, p.Message.Role}, p.InputIDs...) {
		if s != "" {
			f = append(f, s)
		}
	}
	if p.Epoch > 0 || p.Count > 0 {
		f = append(f, fmt.Sprint(p.Epoch+uint64(p.Count)))
	}
	for _, part := range p.Message.Parts {
		f = append(f, strings.TrimSpace(part.CallID+" "+part.Text))
	}
	return strings.Join(f, " ")
}

func wantLog(t *testing.T, st harness.Store, after uint64, want ...string) {
	t.Helper()
	got := logOf(t, st, after)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("log after %d =\n%s\nwant\n%s", after, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func noRun(t *testing.T, f *fake) {
	t.Helper()
	synctest.Wait()
	select {
	case r := <-f.runs:
		t.Fatalf("turn %s started", r.req.TurnID)
	default:
	}
}

func TestSubmitRunsATurn(t *testing.T) {
	eachStore(t, func(t *testing.T, open func() harness.Store) {
		st, f := open(), newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		if got := submit(t, s, text("a", "hi")); got != (protocol.Admitted{InputID: "a", Seq: 3}) {
			t.Fatalf("Submit = %+v", got)
		}
		run := <-f.runs
		if run.req.Input[0].Parts[0].Text != "hi" {
			t.Fatalf("Request.Input = %+v", run.req.Input)
		}
		run.emit(say("hello"))
		run.end()
		wantLog(t, st, 0, "session.created", "owner.acquired 1", "input.admitted a", "turn.started a",
			"item.completed assistant hello", "turn.ended completed")
		v := s.View()
		if v.Status != protocol.StatusIdle || v.HeadSeq != 6 || v.Usage.InputTokens != 3 {
			t.Fatalf("View = %+v", v)
		}
		closeRuntime(t, r)
	})
}

func TestSubmitIsIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, open func() harness.Store) {
		st, f := open(), newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		first := submit(t, s, text("a", "hi"))
		run := <-f.runs
		if again := submit(t, s, text("a", "hi")); again != first {
			t.Fatalf("repeated Submit = %+v, want %+v", again, first)
		}
		if _, err := s.Submit(bg, text("a", "other")); !errors.Is(err, harness.ErrInputConflict) {
			t.Fatalf("Submit with another body = %v, want ErrInputConflict", err)
		}
		run.end()
		wantLog(t, st, 2, "input.admitted a", "turn.started a", "turn.ended completed")
		closeRuntime(t, r)
	})
}

func TestQueuedInputsRunInOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish func(t *testing.T, s *harness.Session, run fakeRun)
		want   []string
	}{
		{"a turn that ends starts the next input", func(t *testing.T, _ *harness.Session, run fakeRun) {
			run.emit(toolResult("c1"))
			run.end()
		}, []string{"item.completed tool c1 ok", "turn.ended completed"}},
		{"an interrupt keeps the partial and starts the next input", func(t *testing.T, s *harness.Session, run fakeRun) {
			if err := s.Interrupt(bg, protocol.Interrupt{TurnID: run.req.TurnID}); err != nil {
				t.Fatal(err)
			}
		}, []string{"item.completed tool c1 " + interrupted, "turn.ended interrupted stopped"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, open func() harness.Store) {
				st, f := open(), newFake()
				r := runtime(t, st, f)
				s := create(t, r)
				submit(t, s, text("a", "one"))
				run := <-f.runs
				run.emit(say("partial"))
				run.emit(callTool("c1"))
				submit(t, s, text("b", "two"))
				if q := s.View().Queued; len(q) != 1 || q[0] != "b" {
					t.Fatalf("Queued = %v, want [b]", q)
				}
				tc.finish(t, s, run)
				next := <-f.runs
				next.end()
				want := append([]string{"input.admitted a", "turn.started a", "item.completed assistant partial",
					"item.completed assistant c1", "input.admitted b"}, tc.want...)
				wantLog(t, st, 2, append(want, "turn.started b", "turn.ended completed")...)
				closeRuntime(t, r)
			})
		})
	}
}
