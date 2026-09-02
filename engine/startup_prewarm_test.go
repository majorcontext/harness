package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type startupPrewarmProvider struct {
	name string

	prewarmRequests chan *provider.Request
	prewarmReturned chan struct{}
	release         <-chan struct{}
	prewarmErr      error
	returnOnce      sync.Once

	mu             sync.Mutex
	streamRequests []*provider.Request
}

func newStartupPrewarmProvider(name string) *startupPrewarmProvider {
	return &startupPrewarmProvider{
		name:            name,
		prewarmRequests: make(chan *provider.Request, 1),
		prewarmReturned: make(chan struct{}),
	}
}

func (p *startupPrewarmProvider) Name() string { return p.name }

func (p *startupPrewarmProvider) Prewarm(ctx context.Context, req *provider.Request) error {
	p.prewarmRequests <- cloneStartupRequest(req)
	defer p.returnOnce.Do(func() { close(p.prewarmReturned) })
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.prewarmErr
}

func (p *startupPrewarmProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.streamRequests = append(p.streamRequests, cloneStartupRequest(req))
	p.mu.Unlock()
	msg := &message.Message{ID: "msg_ready", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "ready"}}}
	return &scriptedStream{events: []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 3, OutputTokens: 2}}}}, nil
}

func (p *startupPrewarmProvider) streams() []*provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*provider.Request(nil), p.streamRequests...)
}

func cloneStartupRequest(req *provider.Request) *provider.Request {
	cp := *req
	cp.System = append([]string(nil), req.System...)
	cp.Messages = append([]message.Message(nil), req.Messages...)
	cp.Tools = append([]provider.ToolDef(nil), req.Tools...)
	return &cp
}

func startupConfig(p provider.Provider) Config {
	return Config{
		Providers: provider.Registry{p.Name(): p},
		Model:     message.ModelRef{Provider: p.Name(), Model: "m1"},
		System:    []string{"base"},
	}
}

func requirePrewarmRequest(t *testing.T, p *startupPrewarmProvider) *provider.Request {
	t.Helper()
	synctest.Wait()
	select {
	case req := <-p.prewarmRequests:
		return req
	default:
		t.Fatal("startup prewarm did not start")
		return nil
	}
}

func TestStartupPrewarmBareSessionWithManagerStartsAfterLocalConstruction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartupPrewarmProvider("test")
		cfg := startupConfig(p)
		cfg.SessionManager = NewSessionManager(context.Background(), 3, 20)
		NewSession(cfg)
		requirePrewarmRequest(t, p)
		<-p.prewarmReturned
	})
}

func TestNewSessionReturnsWhileStartupPrewarmBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		p := newStartupPrewarmProvider("test")
		p.release = release

		s := NewSession(startupConfig(p))
		if s == nil {
			t.Fatal("NewSession returned nil")
		}
		requirePrewarmRequest(t, p)
		close(release)
		<-p.prewarmReturned
	})
}

func TestFirstPromptConsumesReadyStartupPrewarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartupPrewarmProvider("test")
		s := NewSession(startupConfig(p))
		requirePrewarmRequest(t, p)
		<-p.prewarmReturned

		got, err := s.Prompt(context.Background(), "hello")
		if err != nil {
			t.Fatal(err)
		}
		if got.Parts.Text() != "ready" {
			t.Fatalf("Prompt text = %q, want ready", got.Parts.Text())
		}
		if got := len(p.streams()); got != 1 {
			t.Fatalf("Stream calls = %d, want 1", got)
		}
	})
}

func TestFirstPromptWaitsOnlyForRemainingPrewarmDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		p := newStartupPrewarmProvider("test")
		p.release = block
		s := NewSession(startupConfig(p))
		requirePrewarmRequest(t, p)

		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		<-timer.C
		started := time.Now()
		if _, err := s.Prompt(context.Background(), "hello"); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(started); got != 5*time.Second {
			t.Fatalf("first prompt prewarm wait = %s, want remaining 5s", got)
		}
	})
}

func TestPromptCancellationCancelsStartupPrewarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		p := newStartupPrewarmProvider("test")
		p.release = block
		s := NewSession(startupConfig(p))
		requirePrewarmRequest(t, p)

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := s.Prompt(ctx, "hello")
			result <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt error = %v, want context canceled", err)
		}
		<-p.prewarmReturned
	})
}

func TestStartupPrewarmFailureDoesNotFailPrompt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartupPrewarmProvider("test")
		p.prewarmErr = errors.New("prewarm unavailable")
		s := NewSession(startupConfig(p))
		requirePrewarmRequest(t, p)
		<-p.prewarmReturned

		if _, err := s.Prompt(context.Background(), "hello"); err != nil {
			t.Fatalf("Prompt inherited prewarm error: %v", err)
		}
	})
}

func TestStartupPrewarmCachesInstructionAndSkillErrors(t *testing.T) {
	t.Run("instructions", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			work := t.TempDir()
			path := filepath.Join(work, "AGENTS.md")
			if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
				t.Fatal(err)
			}
			p := newStartupPrewarmProvider("test")
			cfg := startupConfig(p)
			cfg.WorkDir = work
			s := NewSession(cfg)
			synctest.Wait()
			if err := os.WriteFile(path, []byte("now valid"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Prompt(context.Background(), "hello"); err == nil || !errors.Is(err, s.instrErr) {
				t.Fatalf("Prompt error = %v, want cached instruction error %v", err, s.instrErr)
			}
		})
	})

	t.Run("skills", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			work := t.TempDir()
			dir := filepath.Join(work, "skills")
			path := filepath.Join(dir, "broken", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not frontmatter"), 0o600); err != nil {
				t.Fatal(err)
			}
			p := newStartupPrewarmProvider("test")
			cfg := startupConfig(p)
			cfg.WorkDir = work
			cfg.SkillsDirs = []string{dir}
			s := NewSession(cfg)
			synctest.Wait()
			if err := os.WriteFile(path, []byte("---\nname: fixed\ndescription: fixed\n---\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Prompt(context.Background(), "hello"); err == nil || !errors.Is(err, s.skillsErr) {
				t.Fatalf("Prompt error = %v, want cached skill error %v", err, s.skillsErr)
			}
		})
	})
}

func TestStartupPrewarmPropertyDriftFallsBackFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartupPrewarmProvider("test")
		s := NewSession(startupConfig(p))
		warm := requirePrewarmRequest(t, p)
		<-p.prewarmReturned
		s.SetModel(message.ModelRef{Provider: "test", Model: "m2"})

		if _, err := s.Prompt(context.Background(), "fresh input"); err != nil {
			t.Fatal(err)
		}
		real := p.streams()[0]
		if warm.Model.Model != "m1" || real.Model.Model != "m2" {
			t.Fatalf("models = warm %s, real %s; want m1 then m2", warm.Model, real.Model)
		}
		if len(warm.Messages) != 0 || len(real.Messages) != 1 || real.Messages[0].Parts.Text() != "fresh input" {
			t.Fatalf("messages = warm %#v, real %#v; want empty then full user input", warm.Messages, real.Messages)
		}
	})
}

func TestStartupPrewarmEmitsNoTurnMessageOrUsage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartupPrewarmProvider("test")
		var events []Event
		var turns []int
		cfg := startupConfig(p)
		cfg.OnEvent = func(ev Event) { events = append(events, ev) }
		cfg.OnRequest = func(_ string, turn int, _ *provider.Request) { turns = append(turns, turn) }
		s := NewSession(cfg)
		requirePrewarmRequest(t, p)
		<-p.prewarmReturned

		if got := s.History(); len(got) != 0 {
			t.Fatalf("history after prewarm = %#v, want empty", got)
		}
		if got := s.Usage(); got != (provider.Usage{}) {
			t.Fatalf("usage after prewarm = %+v, want zero", got)
		}
		if len(events) != 0 || len(turns) != 0 {
			t.Fatalf("prewarm events/turns = %d/%v, want none", len(events), turns)
		}
		if _, err := s.Prompt(context.Background(), "hello"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(turns, []int{1}) {
			t.Fatalf("real turn numbers = %v, want [1]", turns)
		}
	})
}

func TestChildPrewarmStartsAfterToolRestriction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rootProvider := &scriptedProvider{name: "root", turns: doneTurn("root done")}
		childProvider := newStartupPrewarmProvider("child")
		block := make(chan struct{})
		childProvider.release = block
		mgr := NewSessionManager(context.Background(), 3, 20)
		root := mgr.NewRoot(managedConfig("root", rootProvider, childProvider))

		_, err := mgr.Spawn(SpawnOptions{
			ParentID: root.ID,
			Prompt:   "work",
			Model:    modelFor("child"),
			ToolNames: []string{
				"read_file",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := requirePrewarmRequest(t, childProvider)
		if len(req.Tools) != 1 || req.Tools[0].Name != "read_file" {
			t.Fatalf("child prewarm tools = %#v, want only read_file", req.Tools)
		}
		if got := len(childProvider.streams()); got != 0 {
			t.Fatalf("child Stream calls before prewarm resolves = %d, want 0", got)
		}
		if err := mgr.Cancel(root.ID); err != nil {
			t.Fatal(err)
		}
		close(block)
		synctest.Wait()
	})
}

var _ provider.StartupPrewarmer = (*startupPrewarmProvider)(nil)
var _ provider.Provider = (*startupPrewarmProvider)(nil)
