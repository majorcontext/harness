package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
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
// With ownsLoop, the test reports the tool results; without it, the turn
// loop runs the tools. With save, each Run reads its saved state into
// fakeRun.state, then saves save.
type fake struct {
	ownsLoop bool
	save     string
	steering bool
	deaf     bool
	late     []eventlog.Message
	stuck    chan struct{}
	runs     chan fakeRun
}

type fakeRun struct {
	req   turn.Request
	items chan<- eventlog.Message
	tele  chan<- turn.Telemetry
	state string
}

func newFake() *fake { return &fake{ownsLoop: true, runs: make(chan fakeRun)} }

func (f *fake) Capabilities(string) turn.Capabilities {
	return turn.Capabilities{OwnsLoop: f.ownsLoop, Steering: f.steering}
}

func (f *fake) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	items, tele, run := make(chan eventlog.Message), make(chan turn.Telemetry), fakeRun{req: req}
	run.items, run.tele = items, tele
	if f.save != "" {
		run.state = f.loadAndSave(out)
	}
	select {
	case f.runs <- run:
	case <-ctx.Done():
		return turn.Result{}, context.Cause(ctx)
	}
	for {
		select {
		case m, ok := <-items:
			if !ok {
				return turn.Result{}, nil
			}
			if err := f.report(out, m); err != nil {
				return turn.Result{}, err
			}
		case t := <-tele:
			out.Telemetry(t)
		case <-ctx.Done():
			return f.stop(ctx, out)
		}
	}
}

func (f *fake) loadAndSave(out turn.Sink) string {
	prior, err := out.State("fake")
	if err == nil {
		err = out.SaveState("fake", []byte(f.save))
	}
	if err != nil {
		return err.Error()
	}
	return string(prior)
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

// measure reports the usage of one model call.
func (r fakeRun) measure(u eventlog.Usage) {
	r.tele <- turn.Telemetry{Usage: u}
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
	s, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "fake/model", Origin: "test"})
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

// scripted is a Backend whose Run calls the next step, then repeats the last.
type scripted struct {
	steps []func(turn.Sink) error
	runs  atomic.Int32
}

func (b *scripted) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (b *scripted) Run(_ context.Context, _ turn.Request, out turn.Sink) (turn.Result, error) {
	n := int(b.runs.Add(1)) - 1
	return turn.Result{}, b.steps[min(n, len(b.steps)-1)](out)
}

func TestARetryableErrorAfterAnItemEndsTheTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errFlaky := fmt.Errorf("%w: flaky", turn.ErrRetryable)
		st, retries := harness.NewMemStore(), 3
		b := &scripted{steps: []func(turn.Sink) error{func(out turn.Sink) error { _ = out.Item(callTool("c1")); return errFlaky }}}
		r, err := harness.NewWithBackend(harness.Options{Store: st, Config: config.Config{PromptRetries: &retries}}, b)
		if err != nil {
			t.Fatal(err)
		}
		converse(t, create(t, r), "hi")
		wantLog(t, st, 2, "input.admitted a", "turn.started a", "item.completed assistant c1", "item.completed tool c1 "+cutOff,
			"turn.ended failed "+errFlaky.Error())
		if got := b.runs.Load(); got != 1 {
			t.Errorf("runs = %d, want 1", got)
		}
		closeRuntime(t, r)
	})
}

func TestNewRejectsAKeyThatTheRuntimeIgnores(t *testing.T) {
	n := 1
	for key, cfg := range map[string]config.Config{
		"instructions_mode":          {InstructionsMode: "full"},
		"event_sink":                 {EventSink: &config.EventSinkSpec{URL: "http://127.0.0.1:1"}},
		"snapshot_every_records":     {SnapshotEveryRecords: &n},
		"tool_result_inline_bytes":   {ToolResultInlineBytes: &n},
		"tool_result_retained_bytes": {ToolResultRetainedBytes: &n},
	} {
		t.Run(key, func(t *testing.T) {
			_, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: cfg})
			if !errors.Is(err, harness.ErrInvalidRequest) || !strings.Contains(err.Error(), key) {
				t.Fatalf("New = %v, want ErrInvalidRequest that names %s", err, key)
			}
		})
	}
	for name, cfg := range map[string]config.Config{"session_dir": {SessionDir: "/x"}, "session_sync": {SessionSync: "volume"}, "agent_defs_dirs": {AgentDefsDirs: []string{"a"}}} {
		t.Run(name+" is accepted", func(t *testing.T) {
			if _, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: cfg}); err != nil {
				t.Fatalf("New = %v", err)
			}
		})
	}
}
