package engine

import (
	"context"
	"encoding/json"
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
	openaiadapter "github.com/majorcontext/harness/provider/openai"
)

type startupPrewarmProvider struct {
	name string

	prewarmRequests    chan *provider.Request
	prewarmReturned    chan struct{}
	release            <-chan struct{}
	prewarmErr         error
	ignoreCancellation bool
	enabled            bool
	returnOnce         sync.Once

	requestMetadata *provider.RequestMetadata

	mu             sync.Mutex
	streamRequests []*provider.Request
}

func newStartupPrewarmProvider(name string) *startupPrewarmProvider {
	return &startupPrewarmProvider{
		name:            name,
		enabled:         true,
		prewarmRequests: make(chan *provider.Request, 1),
		prewarmReturned: make(chan struct{}),
	}
}

func (p *startupPrewarmProvider) Name() string { return p.name }

func (p *startupPrewarmProvider) StartupPrewarmEnabled() bool { return p.enabled }

func (p *startupPrewarmProvider) Prewarm(ctx context.Context, req *provider.Request) error {
	p.prewarmRequests <- cloneStartupRequest(req)
	defer p.returnOnce.Do(func() { close(p.prewarmReturned) })
	if p.release != nil {
		if p.ignoreCancellation {
			<-p.release
		} else {
			select {
			case <-p.release:
			case <-ctx.Done():
				return ctx.Err()
			}
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
	return &scriptedStream{events: []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 3, OutputTokens: 2}, RequestMetadata: p.requestMetadata}}}, nil
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

type startupCountingMCP struct {
	mu    sync.Mutex
	calls int
}

func (m *startupCountingMCP) Tools(context.Context) []provider.ToolDef {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return nil
}

func (*startupCountingMCP) CallTool(context.Context, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}

func (*startupCountingMCP) CallServerTool(context.Context, string, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}

func (m *startupCountingMCP) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func TestGenericOpenAIStartupPrewarmDoesNoEarlyAssembly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		work := t.TempDir()
		if err := os.WriteFile(filepath.Join(work, "AGENTS.md"), []byte("instructions"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := &openaiadapter.Client{Family: openaiadapter.Family, UseWebSocketTransport: true}
		hooks := &fakeHooks{segments: []string{"hook"}}
		mcp := &startupCountingMCP{}
		cfg := startupConfig(p)
		cfg.WorkDir = work
		cfg.Hooks = hooks
		cfg.MCP = mcp

		s := NewSession(cfg)
		synctest.Wait()

		s.mu.Lock()
		instructionsLoaded := s.instrLoaded
		skillsLoaded := s.skillsLoaded
		s.mu.Unlock()
		if instructionsLoaded || skillsLoaded {
			t.Fatalf("early discovery = instructions:%v skills:%v, want neither", instructionsLoaded, skillsLoaded)
		}
		if hooks.paramCalls != 0 || hooks.systemCalls != 0 {
			t.Fatalf("early hook calls = params:%d system:%d, want zero", hooks.paramCalls, hooks.systemCalls)
		}
		if got := mcp.count(); got != 0 {
			t.Fatalf("early MCP Tools calls = %d, want zero", got)
		}
	})
}

func TestStartupPrewarmBothReadyAndPromptCanceledDoesNotMutate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for i := 0; i < 100; i++ {
			p := newStartupPrewarmProvider("test")
			s := NewSession(startupConfig(p))
			requirePrewarmRequest(t, p)
			<-p.prewarmReturned
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, err := s.Prompt(ctx, "must not append")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("iteration %d: Prompt error = %v, want context.Canceled", i, err)
			}
			if got := s.History(); len(got) != 0 {
				t.Fatalf("iteration %d: history = %#v, want empty", i, got)
			}
			if got := len(p.streams()); got != 0 {
				t.Fatalf("iteration %d: Stream calls = %d, want zero", i, got)
			}
		}
	})
}

func TestStartupPrewarmTimedOutOutcomeCannotBecomeConsumedOrStale(t *testing.T) {
	metrics := make(chan StartupPrewarmMetrics, 2)
	s := newSession(Config{OnStartupPrewarmMetrics: func(m StartupPrewarmMetrics) { metrics <- m }})
	s.ID = "session"
	h := &startupPrewarm{
		startedAt:    time.Unix(0, 0),
		cancel:       func() {},
		deadlineDone: make(chan struct{}),
		done:         make(chan struct{}),
	}
	s.startupPrewarm = h

	if !h.claimOutcome(StartupPrewarmTimedOut, time.Unix(1, 0)) {
		t.Fatal("timed-out outcome did not win test setup")
	}
	s.emitStartupPrewarmMetrics(h, StartupPrewarmTimedOut, time.Unix(1, 0))
	close(h.done)
	if err := s.consumeStartupPrewarm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.startupPrewarmResolution != nil {
		t.Fatal("timed-out outcome installed a usable prewarm resolution")
	}
	s.resolveStartupPrewarm(&provider.RequestMetadata{
		Mode:                 provider.RequestModeIncremental,
		PreviousResponseUsed: true,
	})

	first := <-metrics
	if first.Status != StartupPrewarmTimedOut {
		t.Fatalf("first status = %q, want timed_out", first.Status)
	}
	select {
	case extra := <-metrics:
		t.Fatalf("status sequence = timed_out -> %s, want timed_out only", extra.Status)
	default:
	}
}

func TestStartupPrewarmOutcomeCallbackCanReenterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartupPrewarmProvider("test")
		release := make(chan struct{})
		p.release = release
		var s *Session
		callbackReturned := make(chan struct{})
		cfg := startupConfig(p)
		cfg.OnStartupPrewarmMetrics = func(m StartupPrewarmMetrics) {
			if m.Status == StartupPrewarmReady {
				s.cancelStartupPrewarm()
				close(callbackReturned)
			}
		}
		s = NewSession(cfg)
		requirePrewarmRequest(t, p)
		close(release)
		synctest.Wait()

		select {
		case <-callbackReturned:
		default:
			t.Fatal("reentrant metrics callback did not return")
		}
		s.mu.Lock()
		retained := s.startupPrewarm != nil
		s.mu.Unlock()
		if retained {
			t.Fatal("reentrant cancellation retained startup-prewarm ownership")
		}
	})
}

func TestStartupPrewarmBlockingOutcomeCallbackCannotRetainOwnership(t *testing.T) {
	t.Run("prompt cancellation", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var s *Session
		s = newSession(Config{OnStartupPrewarmMetrics: func(m StartupPrewarmMetrics) {
			if m.Status == StartupPrewarmCancelled {
				close(entered)
				<-release
			}
		}})
		s.ID = "session"
		cancelled := make(chan struct{})
		h := &startupPrewarm{
			startedAt:    time.Now(),
			cancel:       func() { close(cancelled) },
			deadlineDone: make(chan struct{}),
			done:         make(chan struct{}),
		}
		s.startupPrewarm = h
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := make(chan error, 1)
		go func() {
			result <- s.consumeStartupPrewarm(ctx)
		}()
		<-entered

		s.mu.Lock()
		retained := s.startupPrewarm != nil
		s.mu.Unlock()
		select {
		case <-cancelled:
		default:
			retained = true
		}
		close(release)
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("consume error = %v, want context.Canceled", err)
		}
		if retained {
			t.Fatal("blocking cancellation callback retained startup-prewarm ownership")
		}
	})

	t.Run("deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			block := make(chan struct{})
			p := newStartupPrewarmProvider("test")
			p.release = block
			entered := make(chan struct{})
			release := make(chan struct{})
			cfg := startupConfig(p)
			cfg.OnStartupPrewarmMetrics = func(m StartupPrewarmMetrics) {
				if m.Status == StartupPrewarmTimedOut {
					close(entered)
					<-release
				}
			}
			s := NewSession(cfg)
			requirePrewarmRequest(t, p)
			result := make(chan error, 1)
			go func() {
				_, err := s.Prompt(context.Background(), "fallback")
				result <- err
			}()
			synctest.Wait()
			<-entered
			synctest.Wait()

			s.mu.Lock()
			retained := s.startupPrewarm != nil
			s.mu.Unlock()
			select {
			case <-p.prewarmReturned:
			default:
				retained = true
			}
			close(release)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if retained {
				t.Fatal("blocking deadline callback retained startup-prewarm ownership or worker")
			}
		})
	})
}

func TestStartupPrewarmMetricsExposeLifecycleAndResolution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartupPrewarmProvider("test")
		release := make(chan struct{})
		p.release = release
		p.requestMetadata = &provider.RequestMetadata{
			Mode:                 provider.RequestModeIncremental,
			PreviousResponseUsed: true,
		}
		metrics := make(chan StartupPrewarmMetrics, 3)
		cfg := startupConfig(p)
		cfg.OnStartupPrewarmMetrics = func(m StartupPrewarmMetrics) { metrics <- m }
		s := NewSession(cfg)
		requirePrewarmRequest(t, p)
		readyTimer := time.NewTimer(2 * time.Second)
		defer readyTimer.Stop()
		<-readyTimer.C
		close(release)
		<-p.prewarmReturned
		promptTimer := time.NewTimer(3 * time.Second)
		defer promptTimer.Stop()
		<-promptTimer.C
		if _, err := s.Prompt(context.Background(), "hello"); err != nil {
			t.Fatal(err)
		}

		want := []StartupPrewarmStatus{StartupPrewarmStarted, StartupPrewarmReady, StartupPrewarmConsumed}
		for i, status := range want {
			got := <-metrics
			if got.Status != status {
				t.Fatalf("metrics[%d].Status = %q, want %q", i, got.Status, status)
			}
			if got.SessionID != s.ID {
				t.Fatalf("metrics[%d].SessionID = %q, want %q", i, got.SessionID, s.ID)
			}
			if got.DurationMillis < 0 || got.AgeMillis < 0 {
				t.Fatalf("metrics[%d] timings = duration:%d age:%d, want non-negative", i, got.DurationMillis, got.AgeMillis)
			}
			switch status {
			case StartupPrewarmStarted:
				if got.DurationMillis != 0 || got.AgeMillis != 0 {
					t.Fatalf("started timings = duration:%d age:%d, want 0/0", got.DurationMillis, got.AgeMillis)
				}
			case StartupPrewarmReady:
				if got.DurationMillis != 2000 || got.AgeMillis != 2000 {
					t.Fatalf("ready timings = duration:%d age:%d, want 2000/2000", got.DurationMillis, got.AgeMillis)
				}
			case StartupPrewarmConsumed:
				if got.DurationMillis != 2000 || got.AgeMillis != 5000 {
					t.Fatalf("consumed timings = duration:%d age:%d, want 2000/5000", got.DurationMillis, got.AgeMillis)
				}
			}
		}
	})
}

func TestStartupPrewarmMetricsExposeFailureTimeoutCancellationAndStale(t *testing.T) {
	tests := []struct {
		name   string
		status StartupPrewarmStatus
		run    func(*testing.T, *startupPrewarmProvider, *Session)
	}{
		{
			name:   "failed",
			status: StartupPrewarmFailed,
			run: func(t *testing.T, p *startupPrewarmProvider, _ *Session) {
				requirePrewarmRequest(t, p)
				<-p.prewarmReturned
			},
		},
		{
			name:   "timed out",
			status: StartupPrewarmTimedOut,
			run: func(t *testing.T, p *startupPrewarmProvider, s *Session) {
				requirePrewarmRequest(t, p)
				if _, err := s.Prompt(context.Background(), "fallback"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "cancelled",
			status: StartupPrewarmCancelled,
			run: func(t *testing.T, p *startupPrewarmProvider, s *Session) {
				requirePrewarmRequest(t, p)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, _ = s.Prompt(ctx, "cancel")
			},
		},
		{
			name:   "stale",
			status: StartupPrewarmStale,
			run: func(t *testing.T, p *startupPrewarmProvider, s *Session) {
				p.requestMetadata = &provider.RequestMetadata{Mode: provider.RequestModeFull}
				requirePrewarmRequest(t, p)
				<-p.prewarmReturned
				if _, err := s.Prompt(context.Background(), "changed"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newStartupPrewarmProvider("test")
				if tt.name == "failed" {
					p.prewarmErr = errors.New("prewarm failed")
				}
				if tt.name == "timed out" || tt.name == "cancelled" {
					p.release = make(chan struct{})
				}
				metrics := make(chan StartupPrewarmMetrics, 4)
				cfg := startupConfig(p)
				cfg.OnStartupPrewarmMetrics = func(m StartupPrewarmMetrics) { metrics <- m }
				s := NewSession(cfg)
				tt.run(t, p, s)
				synctest.Wait()

				<-metrics // started
				var got StartupPrewarmMetrics
				for len(metrics) > 0 {
					got = <-metrics
				}
				if got.Status != tt.status {
					t.Fatalf("final prewarm status = %q, want %q", got.Status, tt.status)
				}
				if got.DurationMillis < 0 || got.AgeMillis < 0 {
					t.Fatalf("timings = duration:%d age:%d, want non-negative", got.DurationMillis, got.AgeMillis)
				}
			})
		})
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

func TestFirstPromptDeadlineDetachesNoncooperativeStartupPrewarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		p := newStartupPrewarmProvider("test")
		p.release = release
		p.ignoreCancellation = true
		s := NewSession(startupConfig(p))
		requirePrewarmRequest(t, p)

		go func() {
			timer := time.NewTimer(startupPrewarmTimeout + time.Second)
			defer timer.Stop()
			<-timer.C
			close(release)
		}()
		started := time.Now()
		got, err := s.Prompt(context.Background(), "hello")
		elapsed := time.Since(started)

		s.mu.Lock()
		retained := s.startupPrewarm != nil
		s.mu.Unlock()
		if err != nil {
			t.Fatalf("Prompt error = %v, want normal prompt after prewarm deadline", err)
		}
		if got == nil || got.Parts.Text() != "ready" {
			t.Fatalf("Prompt result = %#v, want ready", got)
		}
		if elapsed != startupPrewarmTimeout {
			t.Fatalf("Prompt elapsed = %s, want %s", elapsed, startupPrewarmTimeout)
		}
		if retained {
			t.Fatal("session retained startup-prewarm handle after deadline")
		}
		<-p.prewarmReturned
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
