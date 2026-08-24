package engine

import (
	"context"
	"strings"
	"testing"
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
	waitForState(t, mgr, "db", process.StateExited, 3*time.Second)

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

func waitForState(t *testing.T, m *process.Manager, name string, want process.State, deadline time.Duration) process.Status {
	t.Helper()
	end := time.Now().Add(deadline)
	var last process.Status
	for time.Now().Before(end) {
		st, err := m.Status(name)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		last = st
		if st.State == want {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("process %q did not reach state %q within %s (last: %+v)", name, want, deadline, last)
	return last
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
	last := m.Parts[len(m.Parts)-1]
	ec, ok := last.(*message.EngineContext)
	if !ok {
		t.Fatalf("newest user message's last part = %T, want *message.EngineContext (a forgeable Text is the spoof surface)", last)
	}
	if !strings.Contains(ec.Text, "9.9.9-test") {
		t.Errorf("engine context part text = %q, want the engine identity block", ec.Text)
	}
	// The user's own prompt stays a plain Text part — only the appended
	// ambient block is an EngineContext.
	if _, ok := m.Parts[0].(*message.Text); !ok {
		t.Errorf("user's own prompt part = %T, want *message.Text", m.Parts[0])
	}
}
