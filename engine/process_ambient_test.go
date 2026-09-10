package engine

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/provider"
)

// lastUserText returns the text of the last part of the last RoleUser
// message in req.Messages, for asserting on the ambient status block. The
// ambient block is a *message.EngineContext part (see withAmbientStatus), not
// a *message.Text — this helper reads either so the assertions below see the
// block's text regardless of which part-kind carries it.
func lastUserText(t *testing.T, req *provider.Request) string {
	t.Helper()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != message.RoleUser {
			continue
		}
		m := req.Messages[i]
		switch p := m.Parts[len(m.Parts)-1].(type) {
		case *message.EngineContext:
			return p.Text
		case *message.Text:
			return p.Text
		default:
			t.Fatalf("last part of last user message is neither engine context nor text: %#v", m.Parts[len(m.Parts)-1])
		}
	}
	t.Fatal("no user message in request")
	return ""
}

func TestAmbientProcessStatusAbsentBeforeAnyStart(t *testing.T) {
	dir := t.TempDir()
	mgr := process.NewManager(dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:   dir,
		Processes: mgr,
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(prov.requests))
	}
	if strings.Contains(lastUserText(t, prov.requests[0]), "[processes:") {
		t.Fatal("ambient status block present before any process was ever started")
	}
}

func TestAmbientProcessStatusPresentAfterStart(t *testing.T) {
	dir := t.TempDir()
	mgr := process.NewManager(dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "echo started; sleep 100"}},
	})
	ctx := context.Background()
	if _, err := mgr.Start(ctx, "dev"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(ctx, "dev") })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:   dir,
		Processes: mgr,
	})
	if _, err := s.Prompt(ctx, "hello"); err != nil {
		t.Fatal(err)
	}

	last := lastUserText(t, prov.requests[0])
	if strings.Count(last, "[processes:") != 1 {
		t.Fatalf("last user message = %q, want exactly one ambient status block", last)
	}
	if !strings.Contains(last, "dev ready") {
		t.Errorf("ambient block = %q, want it to report dev ready", last)
	}
	if !strings.Contains(last, "log=") {
		t.Errorf("ambient block = %q, want a log= path", last)
	}

	// Only the newest (in this case, only) user message carries it —
	// earlier messages must be byte-identical to an uninjected request.
	for i, m := range prov.requests[0].Messages {
		if m.Role != message.RoleUser {
			continue
		}
		if i != len(prov.requests[0].Messages)-1 && strings.Contains(renderMsgText(m), "[processes:") {
			t.Fatalf("ambient status block leaked onto a non-newest message: %+v", m)
		}
	}
}

func renderMsgText(m message.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		switch v := p.(type) {
		case *message.Text:
			b.WriteString(v.Text)
		case *message.EngineContext:
			// The ambient status block rides an EngineContext part, not a
			// Text one — include it so the "leaked onto a non-newest message"
			// and "leaked into persisted history" assertions still see it.
			b.WriteString(v.Text)
		}
	}
	return b.String()
}

func TestAmbientProcessStatusIncludesPorts(t *testing.T) {
	dir := t.TempDir()
	mgr := process.NewManager(dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "echo started; sleep 100"}, Ports: []int{3000}},
	})
	ctx := context.Background()
	if _, err := mgr.Start(ctx, "dev"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(ctx, "dev") })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:   dir,
		Processes: mgr,
	})
	if _, err := s.Prompt(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	last := lastUserText(t, prov.requests[0])
	if !strings.Contains(last, "dev ready :3000") {
		t.Errorf("ambient block = %q, want it to report dev's declared port", last)
	}
}

func TestAmbientProcessStatusReflectsExitedState(t *testing.T) {
	dir := t.TempDir()
	mgr := process.NewManager(dir, map[string]process.Def{
		"db": {Command: []string{"sh", "-c", "exit 3"}},
	})
	ctx := context.Background()
	if _, err := mgr.Start(ctx, "db"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForExit(t, mgr, "db")

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:   dir,
		Processes: mgr,
	})
	if _, err := s.Prompt(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	last := lastUserText(t, prov.requests[0])
	if !strings.Contains(last, "db exited(3)") {
		t.Errorf("ambient block = %q, want db exited(3)", last)
	}
}

func TestAmbientProcessStatusNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	sesDir := t.TempDir()
	mgr := process.NewManager(dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "echo started; sleep 100"}},
	})
	ctx := context.Background()
	if _, err := mgr.Start(ctx, "dev"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(ctx, "dev") })

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:    dir,
		SessionDir: sesDir,
		Processes:  mgr,
	})
	if _, err := s.Prompt(ctx, "hello"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(Config{
		Providers:  provider.Registry{"test": prov},
		WorkDir:    dir,
		SessionDir: sesDir,
		Processes:  mgr,
	}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	for _, m := range loaded.History() {
		if strings.Contains(renderMsgText(m), "[processes:") {
			t.Fatalf("ambient status block leaked into persisted history: %+v", m)
		}
	}
}

// waitForExit blocks on process.Manager.WaitExit — the waiter goroutine's
// own completion signal — until name's OS process is gone and its terminal
// state is recorded. Nothing here samples on an interval, so nothing has to
// guess how long to wait between samples.
func waitForExit(t *testing.T, m *process.Manager, name string) {
	t.Helper()
	if _, err := m.WaitExit(context.Background(), name); err != nil {
		t.Fatalf("WaitExit(%q): %v", name, err)
	}
}

// TestAmbientBlockIsEngineContextPart drives the production Prompt entry
// point and proves the ambient status the engine appends to the newest user
// message is a structured *message.EngineContext part, NOT a bare
// *message.Text. This is the canonical-layer half of the trust-spoofing fix
// (see message.EngineContext): a user- or paste-authored Text can never be
// this part-kind, so the block is provably engine-originated.
//
// Red-verify: change withAmbientStatus back to appending a &message.Text and
// this test fails at the type assertion below.
func TestAmbientBlockIsEngineContextPart(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:       dir,
		EngineVersion: "9.9.9-test",
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	req := prov.requests[0]
	m := req.Messages[len(req.Messages)-1]
	if m.Role != message.RoleUser {
		t.Fatalf("newest message role = %q, want user", m.Role)
	}
	if len(m.Parts) != 1 {
		t.Fatalf("pinned ambient message has %d parts, want exactly 1", len(m.Parts))
	}
	ec, ok := m.Parts[0].(*message.EngineContext)
	if !ok {
		t.Fatalf("pinned ambient part = %T, want *message.EngineContext (a forgeable Text is the spoof surface)", m.Parts[0])
	}
	if !strings.Contains(ec.Text, "9.9.9-test") {
		t.Errorf("engine context part text = %q, want the engine identity block", ec.Text)
	}
	prompt := req.Messages[0]
	if _, ok := prompt.Parts[0].(*message.Text); !ok {
		t.Errorf("user's own prompt part = %T, want *message.Text", prompt.Parts[0])
	}
	for _, p := range prompt.Parts {
		if _, ok := p.(*message.EngineContext); ok {
			t.Errorf("ambient block was welded onto the user's own prompt message: %+v", prompt)
		}
	}
}

// fakeProcessRegistry reports one fixed process.Info, so a test can render
// the ambient block twice with no real child process and no state change
// between the two renders.
type fakeProcessRegistry struct{ info process.Info }

func (f *fakeProcessRegistry) Start(context.Context, string) (process.Status, error) {
	return f.info.Status, nil
}
func (f *fakeProcessRegistry) Stop(context.Context, string) (process.Status, error) {
	return f.info.Status, nil
}
func (f *fakeProcessRegistry) Restart(context.Context, string) (process.Status, error) {
	return f.info.Status, nil
}
func (f *fakeProcessRegistry) Status(string) (process.Status, error) { return f.info.Status, nil }
func (f *fakeProcessRegistry) Logs(string, int) (string, process.Status, error) {
	return "", f.info.Status, nil
}
func (f *fakeProcessRegistry) List() []process.Info              { return []process.Info{f.info} }
func (f *fakeProcessRegistry) Declare(string, process.Def) error { return nil }
func (f *fakeProcessRegistry) Undeclare(string) error            { return nil }
func (f *fakeProcessRegistry) EverStarted() bool                 { return true }

// TestAmbientProcessStatusIsStableWhileNothingChanges pins the prompt-cache
// invariant docs/design/managed-processes.md §4 claims: the block rides the
// NEWEST user message, that message stays the newest one for every model
// call of a tool loop, and a Codex WebSocket chain (see
// docs/design/codex-websocket-chaining.md) projects an input suffix only
// while every earlier item is byte-identical. A block whose text changes
// between two calls of one loop therefore costs a whole uncached re-send.
//
// Input: one ready process, unchanged. State: two renders of the ambient
// segment, 90 seconds of session time apart. Wrong output: two different
// strings. Red-verified against the elapsed-time rendering this replaces:
// "dev ready :3000 0s log=..." then "dev ready :3000 1m log=...".
func TestAmbientProcessStatusIsStableWhileNothingChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := &fakeProcessRegistry{info: process.Info{
			Name:  "dev",
			Ports: []int{3000},
			Status: process.Status{
				Name:      "dev",
				State:     process.StateReady,
				StartedAt: time.Now(),
				Ready:     true,
				Log:       "/work/.harness/proc/dev.log",
			},
		}}
		first := processStatusSegment(reg, "/work")
		if !strings.Contains(first, "dev ready") {
			t.Fatalf("ambient block = %q, want it to report dev ready", first)
		}
		// Advances only this bubble's fake clock; no real time passes.
		time.Sleep(90 * time.Second)
		if second := processStatusSegment(reg, "/work"); second != first {
			t.Fatalf("ambient block changed while no process state changed:\n first  = %q\n second = %q", first, second)
		}
	})
}

// TestAmbientProcessStatusReportsAbsoluteInstants states the replacement
// contract the stability test above depends on: each token names WHEN the
// process reached its state, as an absolute UTC RFC3339 instant, never a
// duration relative to the moment the request was assembled.
func TestAmbientProcessStatusReportsAbsoluteInstants(t *testing.T) {
	started := time.Date(2026, 9, 8, 17, 48, 27, 0, time.UTC)
	finished := time.Date(2026, 9, 8, 18, 3, 9, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status process.Status
		want   string
	}{
		{
			name:   "ready",
			status: process.Status{State: process.StateReady, StartedAt: started, Log: "/work/dev.log"},
			want:   "dev ready since 2026-09-08T17:48:27Z log=dev.log",
		},
		{
			name:   "exited",
			status: process.Status{State: process.StateExited, StartedAt: started, FinishedAt: finished, ExitCode: 3, HasExitCode: true, Log: "/work/dev.log"},
			want:   "dev exited(3) at 2026-09-08T18:03:09Z log=dev.log",
		},
		{
			name:   "stopped",
			status: process.Status{State: process.StateStopped, StartedAt: started, FinishedAt: finished, Log: "/work/dev.log"},
			want:   "dev stopped at 2026-09-08T18:03:09Z log=dev.log",
		},
		{
			name:   "never finished still reports no instant",
			status: process.Status{State: process.StateStopped, StartedAt: started, Log: "/work/dev.log"},
			want:   "dev stopped log=dev.log",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatProcessStatus(process.Info{Name: "dev", Status: tc.status}, "/work")
			if got != tc.want {
				t.Fatalf("formatProcessStatus = %q, want %q", got, tc.want)
			}
		})
	}
}
