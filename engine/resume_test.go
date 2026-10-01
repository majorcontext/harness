package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func userMsg(id, text string) message.Message {
	return message.Message{ID: id, Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: text}}}
}

func toolUseMsg(id, callID string) message.Message {
	return message.Message{ID: id, Role: message.RoleAssistant, Parts: message.Parts{toolCall(callID, "probe", `{}`)}}
}

func toolResultMsg(id, callID, text string) message.Message {
	return message.Message{ID: id, Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: callID, Content: message.Parts{&message.Text{Text: text}}},
	}}
}

type probeTool struct {
	mu   sync.Mutex
	runs int
}

func (p *probeTool) tool() Tool {
	return Tool{
		Def: provider.ToolDef{Name: "probe", Description: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, *Session, json.RawMessage) (message.Parts, error) {
			p.mu.Lock()
			p.runs++
			p.mu.Unlock()
			return message.Parts{&message.Text{Text: "real"}}, nil
		},
	}
}

func (p *probeTool) ran() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

func resumeConfig(st SessionStore, prov provider.Provider, maxResumes int, tools ...Tool) Config {
	return Config{
		SessionStore:   st,
		Providers:      provider.Registry{prov.Name(): prov},
		Model:          modelFor(prov.Name()),
		MaxTurnResumes: maxResumes,
		Tools:          tools,
	}
}

func journalOf(t *testing.T, st SessionStore, id string) []record {
	t.Helper()
	lines, err := st.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := make([]record, 0, len(lines))
	for _, l := range lines {
		var r record
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatalf("decode journal line %q: %v", l, err)
		}
		out = append(out, r)
	}
	return out
}

func crashAfter(t *testing.T, st SessionStore, cfg Config, msgs ...message.Message) string {
	t.Helper()
	cfg.SessionStore = st
	s := NewSession(cfg)
	for _, m := range msgs {
		s.append(m)
	}
	return s.ID
}

func reloadSession(t *testing.T, st SessionStore, cfg Config, id string) *Session {
	t.Helper()
	cfg.SessionStore = st
	s, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	return s
}

func lastRequestMessages(t *testing.T, p *scriptedProvider) []message.Message {
	t.Helper()
	if len(p.requests) != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1", len(p.requests))
	}
	return p.requests[0].Messages
}

func TestResumeAfterUserMessage(t *testing.T) {
	st := NewMemStore()
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3)
	id := crashAfter(t, st, cfg, userMsg("u1", "q"))

	s := reloadSession(t, st, cfg, id)
	if !s.ResumableTurn() {
		t.Fatal("ResumableTurn() = false, want true for a journal ending at a user message")
	}
	if got := s.TurnResumes(); got != 0 {
		t.Fatalf("TurnResumes() = %d before resume, want 0", got)
	}
	var events []Event
	s.cfg.OnEvent = func(ev Event) { events = append(events, ev) }

	reply, err := s.ResumeTurn(context.Background())
	if err != nil {
		t.Fatalf("ResumeTurn: %v", err)
	}
	if reply.Parts.Text() != "a" {
		t.Errorf("reply = %q, want %q", reply.Parts.Text(), "a")
	}
	msgs := lastRequestMessages(t, prov)
	last := msgs[len(msgs)-1]
	if last.Role != message.RoleUser || last.Parts.Text() != "q" {
		t.Errorf("last request message = %s %q, want user %q", last.Role, last.Parts.Text(), "q")
	}

	recs := journalOf(t, st, id)
	tail := recs[len(recs)-2:]
	if tail[0].Type != recTurnResumed || tail[0].Count != 1 {
		t.Errorf("record before the reply = %+v, want turn.resumed count 1", tail[0])
	}
	if tail[1].Type != recMessage || tail[1].Message.Role != message.RoleAssistant || tail[1].Message.Parts.Text() != "a" {
		t.Errorf("last record = %+v, want assistant message %q", tail[1], "a")
	}
	if got := s.TurnResumes(); got != 1 {
		t.Errorf("TurnResumes() = %d before settle, want 1", got)
	}
	var resumedEvents []string
	for _, ev := range events {
		if ev.Type == EventTurnResumed {
			resumedEvents = append(resumedEvents, ev.Text)
		}
	}
	if len(resumedEvents) != 1 || resumedEvents[0] != "1" {
		t.Errorf("turn.resumed events = %q, want [\"1\"]", resumedEvents)
	}

	s.markTurnSettled()
	if got := s.TurnResumes(); got != 0 {
		t.Errorf("TurnResumes() = %d after settle, want 0", got)
	}
}

func TestResumeAfterToolResult(t *testing.T) {
	st := NewMemStore()
	probe := &probeTool{}
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3, probe.tool())
	id := crashAfter(t, st, cfg, userMsg("u1", "q"), toolUseMsg("a1", "t1"), toolResultMsg("r1", "t1", "earlier"))

	s := reloadSession(t, st, cfg, id)
	if _, err := s.ResumeTurn(context.Background()); err != nil {
		t.Fatalf("ResumeTurn: %v", err)
	}
	msgs := lastRequestMessages(t, prov)
	last := msgs[len(msgs)-1]
	res, ok := last.Parts[0].(*message.ToolResult)
	if last.Role != message.RoleTool || !ok || res.CallID != "t1" || res.Content.Text() != "earlier" {
		t.Errorf("last request message = %+v, want the journaled tool result for t1", last)
	}
	if probe.ran() != 0 {
		t.Errorf("tool ran %d times, want 0", probe.ran())
	}
}

func TestResumeUnresolvedToolCallsSynthetic(t *testing.T) {
	st := NewMemStore()
	probe := &probeTool{}
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3, probe.tool())
	id := crashAfter(t, st, cfg, userMsg("u1", "q"), toolUseMsg("a1", "t1"))

	s := reloadSession(t, st, cfg, id)
	if _, err := s.ResumeTurn(context.Background()); err != nil {
		t.Fatalf("ResumeTurn: %v", err)
	}
	msgs := lastRequestMessages(t, prov)
	last := msgs[len(msgs)-1]
	prev := msgs[len(msgs)-2]
	if prev.Role != message.RoleAssistant {
		t.Errorf("message before the result = %s, want the assistant tool call", prev.Role)
	}
	res, ok := last.Parts[0].(*message.ToolResult)
	if last.Role != message.RoleTool || !ok || res.CallID != "t1" {
		t.Fatalf("last request message = %+v, want a tool result for t1", last)
	}
	if res.Content.Text() != interruptedTurnErrorText || !res.IsError {
		t.Errorf("result = %q (error=%v), want the interrupted text as an error", res.Content.Text(), res.IsError)
	}
	if probe.ran() != 0 {
		t.Errorf("tool ran %d times, want 0", probe.ran())
	}
}

func TestResumeUnresolvedToolCallsRerun(t *testing.T) {
	st := NewMemStore()
	probe := &probeTool{}
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3, probe.tool())
	cfg.ResumeRerunTools = true
	id := crashAfter(t, st, cfg, userMsg("u1", "q"), toolUseMsg("a1", "t1"))

	s := reloadSession(t, st, cfg, id)
	if _, err := s.ResumeTurn(context.Background()); err != nil {
		t.Fatalf("ResumeTurn: %v", err)
	}
	if probe.ran() != 1 {
		t.Errorf("tool ran %d times, want 1", probe.ran())
	}
	msgs := lastRequestMessages(t, prov)
	last := msgs[len(msgs)-1]
	res, ok := last.Parts[0].(*message.ToolResult)
	if last.Role != message.RoleTool || !ok || res.CallID != "t1" || res.Content.Text() != "real" {
		t.Errorf("last request message = %+v, want the real result %q for t1", last, "real")
	}
}

func TestResumeCapClosesLostToRestart(t *testing.T) {
	st := NewMemStore()
	failing := &resumeFailProvider{err: provider.MarkPermanent(errors.New("boom"))}
	cfg := resumeConfig(st, failing, 3)
	id := crashAfter(t, st, cfg, userMsg("u1", "q"))

	for i := 1; i <= 3; i++ {
		s := reloadSession(t, st, cfg, id)
		if !s.ResumableTurn() {
			t.Fatalf("adoption %d: ResumableTurn() = false, want true", i)
		}
		if _, err := s.ResumeTurn(context.Background()); err == nil {
			t.Fatalf("adoption %d: ResumeTurn succeeded, want the provider error", i)
		}
	}

	s := reloadSession(t, st, cfg, id)
	if got := s.TurnResumes(); got != 3 {
		t.Fatalf("TurnResumes() = %d after three crashed resumes, want 3", got)
	}
	if s.ResumableTurn() {
		t.Fatal("ResumableTurn() = true at the cap, want false")
	}
	mgr := NewSessionManager(context.Background(), 0, 0)
	if err := mgr.AdoptRoot(s); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	hist := s.History()
	if last := hist[len(hist)-1]; !isLostToRestartMarker(last) {
		t.Errorf("last message = %+v, want the lost-to-restart marker", last)
	}
	if s.hasUnfinalizedTurn() {
		t.Error("turn still unsettled after the cap close")
	}
}

type resumeFailProvider struct{ err error }

func (p *resumeFailProvider) Name() string { return "fail" }
func (p *resumeFailProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	return nil, p.err
}

func TestResumeCountDurableBeforeModelCall(t *testing.T) {
	st := NewMemStore()
	blocker := &signaledBlockingProvider{name: "p", started: make(chan struct{}), release: make(chan struct{})}
	cfg := resumeConfig(st, blocker, 3)
	id := crashAfter(t, st, cfg, userMsg("u1", "q"))

	s := reloadSession(t, st, cfg, id)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.ResumeTurn(ctx)
	}()
	<-blocker.started

	reloaded := reloadSession(t, st, cfg, id)
	if got := reloaded.TurnResumes(); got != 1 {
		t.Errorf("TurnResumes() after reload mid-call = %d, want 1", got)
	}
	cancel()
	<-done
}

func TestAbortedTurnNeverResumes(t *testing.T) {
	st := NewMemStore()
	blocker := &signaledBlockingProvider{name: "p", started: make(chan struct{}), release: make(chan struct{})}
	cfg := resumeConfig(st, blocker, 3)
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(withStore(cfg, st))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mgr.Send(context.Background(), root.ID, "q")
	}()
	<-blocker.started
	if err := mgr.AbortTurn(root.ID); err != nil {
		t.Fatalf("AbortTurn: %v", err)
	}
	<-done

	reloaded := reloadSession(t, st, cfg, root.ID)
	if reloaded.ResumableTurn() {
		t.Error("ResumableTurn() = true after AbortTurn, want false")
	}
	if reloaded.hasUnfinalizedTurn() {
		t.Error("hasUnfinalizedTurn() = true after AbortTurn, want false")
	}
}

func withStore(cfg Config, st SessionStore) Config {
	cfg.SessionStore = st
	return cfg
}

func TestCallerCancelLeavesTurnResumable(t *testing.T) {
	st := NewMemStore()
	blocker := &signaledBlockingProvider{name: "p", started: make(chan struct{}), release: make(chan struct{})}
	cfg := resumeConfig(st, blocker, 3)
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(withStore(cfg, st))

	ctx, cancel := context.WithCancel(context.Background())
	var sendErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, sendErr = mgr.Send(ctx, root.ID, "q")
	}()
	<-blocker.started
	cancel()
	<-done
	if !errors.Is(sendErr, context.Canceled) {
		t.Fatalf("Send error = %v, want context.Canceled", sendErr)
	}

	reloaded := reloadSession(t, st, cfg, root.ID)
	if !reloaded.ResumableTurn() {
		t.Error("ResumableTurn() = false after the caller canceled, want true")
	}
}

func TestCallerCancelSettlesWhenResumeIsOff(t *testing.T) {
	st := NewMemStore()
	blocker := &signaledBlockingProvider{name: "p", started: make(chan struct{}), release: make(chan struct{})}
	cfg := resumeConfig(st, blocker, 0)
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(withStore(cfg, st))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mgr.Send(ctx, root.ID, "q")
	}()
	<-blocker.started
	cancel()
	<-done

	if reloadSession(t, st, cfg, root.ID).hasUnfinalizedTurn() {
		t.Error("hasUnfinalizedTurn() = true with MaxTurnResumes 0, want the old settle")
	}
}

func TestManagerResumeTurnRunsAndSettles(t *testing.T) {
	st := NewMemStore()
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3)
	id := crashAfter(t, st, cfg, userMsg("u1", "q"))

	mgr := NewSessionManager(context.Background(), 0, 0)
	s := reloadSession(t, st, cfg, id)
	if err := mgr.AdoptRoot(s); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	if !s.hasUnfinalizedTurn() {
		t.Fatal("AdoptRoot closed a resumable turn")
	}
	reply, err := mgr.ResumeTurn(context.Background(), id)
	if err != nil {
		t.Fatalf("ResumeTurn: %v", err)
	}
	if reply.Parts.Text() != "a" {
		t.Errorf("reply = %q, want %q", reply.Parts.Text(), "a")
	}
	if s.hasUnfinalizedTurn() || s.TurnResumes() != 0 {
		t.Errorf("after settle: unsettled=%v resumes=%d, want false and 0", s.hasUnfinalizedTurn(), s.TurnResumes())
	}
	info, _ := mgr.Info(id)
	if info.Status != StatusIdle {
		t.Errorf("status = %q, want %q", info.Status, StatusIdle)
	}
}

func TestResumeRefusedWhenNotResumable(t *testing.T) {
	st := NewMemStore()
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3)
	s := NewSession(withStore(cfg, st))
	if _, err := s.ResumeTurn(context.Background()); err == nil {
		t.Error("ResumeTurn on a settled session succeeded, want an error")
	}
}

func TestResumeOffByDefault(t *testing.T) {
	st := NewMemStore()
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 0)
	id := crashAfter(t, st, cfg, userMsg("u1", "q"))

	s := reloadSession(t, st, cfg, id)
	if s.ResumableTurn() {
		t.Fatal("ResumableTurn() = true with MaxTurnResumes 0")
	}
	mgr := NewSessionManager(context.Background(), 0, 0)
	if err := mgr.AdoptRoot(s); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	hist := s.History()
	if last := hist[len(hist)-1]; !isLostToRestartMarker(last) {
		t.Errorf("last message = %+v, want the lost-to-restart marker", last)
	}
}

type fakeDelegated struct {
	mu      sync.Mutex
	lastMsg []message.Message
	calls   int
}

func (b *fakeDelegated) SpecificationVersion() string { return "v1" }

func (b *fakeDelegated) RunTurn(_ context.Context, s *Session) (*message.Message, error) {
	b.mu.Lock()
	b.calls++
	b.lastMsg = s.History()
	b.mu.Unlock()
	return &message.Message{ID: "d", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "delegated"}}}, nil
}

func TestResumeDelegatedTrailingAssistant(t *testing.T) {
	backend := &fakeDelegated{}
	orig := delegatedBackends[ClaudeCodeProviderFamily]
	delegatedBackends[ClaudeCodeProviderFamily] = backend
	t.Cleanup(func() { delegatedBackends[ClaudeCodeProviderFamily] = orig })

	cases := []struct {
		name         string
		journal      []message.Message
		wantLastText string
		wantOrigin   string
	}{
		{
			name:         "trailing assistant gets the fixed prompt",
			journal:      []message.Message{userMsg("u1", "q"), toolUseMsg("a1", "t1")},
			wantLastText: delegatedResumeText,
			wantOrigin:   message.OriginEngine,
		},
		{
			name:         "trailing user message gets no extra prompt",
			journal:      []message.Message{userMsg("u1", "q")},
			wantLastText: "q",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := NewMemStore()
			cfg := Config{SessionStore: st, Model: message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"}, MaxTurnResumes: 3}
			id := crashAfter(t, st, cfg, tc.journal...)
			s := reloadSession(t, st, cfg, id)
			if _, err := s.ResumeTurn(context.Background()); err != nil {
				t.Fatalf("ResumeTurn: %v", err)
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			last := backend.lastMsg[len(backend.lastMsg)-1]
			if last.Role != message.RoleUser || last.Parts.Text() != tc.wantLastText || last.Origin != tc.wantOrigin {
				t.Errorf("backend saw last message %s %q origin %q, want user %q origin %q", last.Role, last.Parts.Text(), last.Origin, tc.wantLastText, tc.wantOrigin)
			}
		})
	}
}

func TestResumeNeverForChild(t *testing.T) {
	st := NewMemStore()
	prov := scriptedTurns("p", doneTurn("a")).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3)
	cfg.SessionStore = st
	cfg.TaskParentID = "parent"
	cfg.TaskDepth = 1
	id := crashAfter(t, st, cfg, userMsg("u1", "q"))

	s := reloadSession(t, st, resumeConfig(st, prov, 3), id)
	if s.ResumableTurn() {
		t.Fatal("ResumableTurn() = true for a task child")
	}
	mgr := NewSessionManager(context.Background(), 0, 0)
	if err := mgr.AdoptReloaded(s); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}
	hist := s.History()
	if last := hist[len(hist)-1]; !isLostToRestartMarker(last) {
		t.Errorf("last message = %+v, want the lost-to-restart marker", last)
	}
	if s.hasUnfinalizedTurn() {
		t.Error("child turn still unsettled after recovery")
	}
}

// Step 2 of the crash-window table writes to the ancestor's log, so a
// root's own log at step 2 matches step 1.
func recoveryStep(t *testing.T, st SessionStore, cfg Config, step int) string {
	t.Helper()
	cfg.SessionStore = st
	s := NewSession(cfg)
	s.append(userMsg("u1", "q"))
	if step >= 1 {
		n := taskNotification{ChildID: s.ID, Status: StatusFailed, FailReason: "lost to restart: turn was in flight when the process last stopped"}
		s.commitTurnOutcome(n)
		s.persistCommittedTurnOutcome(n)
	}
	if step >= 3 {
		s.append(message.Message{ID: "closer", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: lostToRestartText}}})
	}
	if step >= 4 {
		s.markTurnSettled()
		s.persistTurnSettled()
	}
	return s.ID
}

func TestResumeRecoveryTable(t *testing.T) {
	cases := []struct {
		step          int
		wantResumable bool
		wantClosers   int
	}{
		{step: 0, wantResumable: true, wantClosers: 0},
		{step: 1, wantClosers: 1},
		{step: 2, wantClosers: 1},
		{step: 3, wantClosers: 1},
		{step: 4, wantClosers: 1},
	}
	for _, tc := range cases {
		t.Run("step"+strconv.Itoa(tc.step), func(t *testing.T) {
			st := NewMemStore()
			prov := scriptedTurns("p", doneTurn("a"))
			cfg := resumeConfig(st, prov, 3)
			id := recoveryStep(t, st, cfg, tc.step)

			s := reloadSession(t, st, cfg, id)
			if got := s.ResumableTurn(); got != tc.wantResumable {
				t.Fatalf("ResumableTurn() = %v, want %v", got, tc.wantResumable)
			}
			mgr := NewSessionManager(context.Background(), 0, 0)
			if err := mgr.AdoptRoot(s); err != nil {
				t.Fatalf("AdoptRoot: %v", err)
			}
			if got := s.hasUnfinalizedTurn(); got != tc.wantResumable {
				t.Errorf("hasUnfinalizedTurn() = %v after AdoptRoot, want %v", got, tc.wantResumable)
			}
			closers := 0
			for _, m := range s.History() {
				if isLostToRestartMarker(m) {
					closers++
				}
			}
			if closers != tc.wantClosers {
				t.Errorf("history holds %d lost-to-restart markers, want %d", closers, tc.wantClosers)
			}
		})
	}
}
