package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// multiProviderHarness builds a harness whose Config.Providers holds every
// entry in providers, keyed by each Provider's own Name() — needed because
// server_test.go's newServer/mkCfg wires exactly one provider, and these
// tests need a parent and a child (or several children) to run distinct
// scripted turns. Deliberately does not wire OnEvent: these tests poll GET
// /session directly rather than the SSE journal.
func multiProviderHarness(t *testing.T, model message.ModelRef, mutate func(*Options), providers ...provider.Provider) *harness {
	t.Helper()
	dir := t.TempDir()
	reg := make(provider.Registry, len(providers))
	for _, p := range providers {
		reg[p.Name()] = p
	}
	opts := Options{
		SessionDir: dir,
		RunToken:   "secret-run-token",
		Version:    "9.9.9",
		NewSession: func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
			if m.IsZero() {
				m = model
			}
			return engine.NewSession(engine.Config{
				Providers:     reg,
				Model:         m,
				WorkDir:       workDir,
				ParentSession: parentSession,
				SessionDir:    dir,
			}), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(engine.Config{Providers: reg, Model: model, SessionDir: dir}, id)
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
}

func waitForLineageStatus(t *testing.T, h *harness, id, want string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, data := h.do("GET", "/session/"+id, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("GET /session/%s status %d: %s", id, resp.StatusCode, data)
		}
		var got struct {
			Lineage map[string]any `json:"lineage"`
		}
		mustUnmarshal(t, data, &got)
		if got.Lineage == nil {
			t.Fatalf("GET /session/%s: no lineage in response: %s", id, data)
		}
		if got.Lineage["status"] == want {
			return got.Lineage
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /session/%s: lineage.status = %v after %s, want %q", id, got.Lineage["status"], timeout, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSessionCreateWithParentIDSpawnsChild is the session.create
// "with a parent" wire test: POST /session with parent_id set behaves like
// a task call made from outside the model — the child is a real
// SessionManager-tracked session, its lineage visible on GET /session/{id}
// for both itself and its parent.
func TestSessionCreateWithParentIDSpawnsChild(t *testing.T) {
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("the answer is 42")}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID,
		"agent":     engine.AgentExplore,
		"prompt":    "find the answer",
		"model":     "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID      string         `json:"id"`
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &child)
	if child.Lineage["parent_id"] != root.ID {
		t.Errorf("child lineage.parent_id = %v, want %q", child.Lineage["parent_id"], root.ID)
	}
	if child.Lineage["depth"] != float64(1) {
		t.Errorf("child lineage.depth = %v, want 1", child.Lineage["depth"])
	}
	if child.Lineage["agent_type"] != engine.AgentExplore {
		t.Errorf("child lineage.agent_type = %v, want %q", child.Lineage["agent_type"], engine.AgentExplore)
	}

	lineage := waitForLineageStatus(t, h, child.ID, "done", 2*time.Second)
	if lineage["result"] != "the answer is 42" {
		t.Errorf("child lineage.result = %v, want %q", lineage["result"], "the answer is 42")
	}

	resp, data = h.do("GET", "/session/"+root.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get root status %d: %s", resp.StatusCode, data)
	}
	var rootInfo struct {
		Lineage struct {
			Children []string `json:"children"`
		} `json:"lineage"`
	}
	mustUnmarshal(t, data, &rootInfo)
	found := false
	for _, c := range rootInfo.Lineage.Children {
		if c == child.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("root lineage.children = %v, want to include %q", rootInfo.Lineage.Children, child.ID)
	}
}

// TestSessionCreateWithParentIDUnknownAgentIs400 proves an unknown agent
// name is rejected before anything is spawned.
func TestSessionCreateWithParentIDUnknownAgentIs400(t *testing.T) {
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil, &scriptedProvider{name: "root"})
	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID,
		"agent":     "not-a-real-agent",
		"prompt":    "go",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown agent status = %d, want 400: %s", resp.StatusCode, data)
	}
}

// TestSessionCreateWithParentIDUnknownParentIs404 proves an unknown
// parent_id is rejected, not silently treated as a fresh root.
func TestSessionCreateWithParentIDUnknownParentIs404(t *testing.T) {
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil, &scriptedProvider{name: "root"})
	resp, data := h.do("POST", "/session", map[string]string{
		"parent_id": "ses_doesnotexist00000000",
		"agent":     engine.AgentExplore,
		"prompt":    "go",
	})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown parent status = %d, want 404: %s", resp.StatusCode, data)
	}
}

// TestSessionSendDeliversToRoot proves session.send (POST
// /session/{id}/send) delivers a message asynchronously and the outcome is
// visible via GET /session/{id}'s ordinary fields (not lineage-specific —
// a plain root has no lineage.result of its own, see finalizeTurn's
// root-always-idle rule).
func TestSessionSendDeliversToRoot(t *testing.T) {
	prov := &scriptedProvider{name: "root", turns: [][]provider.Event{asstTurn("hello back")}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil, prov)

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session/"+root.ID+"/send", map[string]string{"text": "hello"})
	if resp.StatusCode != 202 {
		t.Fatalf("send status %d: %s", resp.StatusCode, data)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, data := h.do("GET", "/session/"+root.ID+"/message", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
		}
		if len(data) > 2 && string(data) != "[]" && strings.Contains(string(data), "hello back") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never received the reply: %s", data)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSessionSendUnknownSessionIs404 proves session.send 404s for an id
// this server's SessionManager does not track.
func TestSessionSendUnknownSessionIs404(t *testing.T) {
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil, &scriptedProvider{name: "root"})
	resp, data := h.do("POST", "/session/ses_doesnotexist00000000/send", map[string]string{"text": "hi"})
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, data)
	}
}

// TestCancelTreeCascadesToChild proves DELETE /session/{id}/cancel_tree
// cancels a running child, wire-exposed cascade cancellation.
func TestCancelTreeCascadesToChild(t *testing.T) {
	blocker := newBlockingProvider("blocker")
	t.Cleanup(blocker.releaseAll)
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, blocker)

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID,
		"agent":     engine.AgentGeneralPurpose,
		"prompt":    "go",
		"model":     "blocker/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)

	waitForLineageStatus(t, h, child.ID, "running", 2*time.Second)

	resp, data = h.do("DELETE", "/session/"+root.ID+"/cancel_tree", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("cancel_tree status %d: %s", resp.StatusCode, data)
	}

	waitForLineageStatus(t, h, child.ID, "canceled", 2*time.Second)
	waitForLineageStatus(t, h, root.ID, "canceled", 2*time.Second)
}

// TestConcurrentPromptDuringResumeIsQueuedNotConcurrent is the server-level
// regression test for the original BLOCKER: an engine-initiated resume
// turn on a root and an ordinary POST /session/{id}/prompt_async request
// arriving while it's in flight must never both drive Session.Prompt at
// once. Before ExternalRunner/ReportTurnStart existed, a resume turn
// SessionManager drove directly left the root's node (and this server's
// own view via a stale read) inconsistent, and a concurrent prompt_async
// could claim the run slot and start a second, overlapping Prompt call —
// reproduced live with -race. Now both go through the exact same
// claimForPrompt admission gate, so the second request must be queued
// (202 "queued"), never started concurrently.
func TestConcurrentPromptDuringResumeIsQueuedNotConcurrent(t *testing.T) {
	// The initial prompt runs on a plain, non-blocking provider; the
	// engine-initiated resume later needs a BLOCKING one so this test can
	// observe it in flight and fire a real concurrent prompt_async against
	// it. Switching the session's PERSISTED model between the two (via
	// POST /session/{id}/model, below) — rather than a per-request model
	// override on the initial prompt, which itself persists via SetModel
	// (see handlePrompt) and would leave the resume ALSO targeting the
	// non-blocking provider — is what makes the resume actually land on
	// resumeBlocker.
	startProv := &scriptedProvider{name: "start", turns: [][]provider.Event{asstTurn("started")}}
	resumeBlocker := newBlockingProvider("resumeblock")
	t.Cleanup(resumeBlocker.releaseAll)
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("child done")}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "start", Model: "m1"}, nil, startProv, resumeBlocker, childProv)

	resp, data := h.do("POST", "/session", map[string]string{"model": "start/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	// First, an ordinary prompt on the plain "start" provider establishes
	// real history and settles the root idle.
	resp, data = h.do("POST", "/session/"+root.ID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "start"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("initial prompt_async status %d: %s", resp.StatusCode, data)
	}
	waitForLineageStatus(t, h, root.ID, "idle", 2*time.Second)

	// Now switch the session's PERSISTED model to the blocking provider —
	// the resume, triggered later by the child's completion, uses
	// whatever the session's current model is at that time.
	resp, data = h.do("POST", "/session/"+root.ID+"/model", map[string]string{"model": "resumeblock/m1"})
	if resp.StatusCode != 200 {
		t.Fatalf("set model status %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID,
		"agent":     engine.AgentGeneralPurpose,
		"prompt":    "go",
		"model":     "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}

	// The child completes fast (one scripted turn) and triggers an
	// engine-initiated resume on the now-idle root (its default model,
	// resumeblock), which claims the run slot and blocks.
	<-resumeBlocker.started
	waitForLineageStatus(t, h, root.ID, "running", 2*time.Second)

	// Now fire a REAL ordinary prompt while the resume is in flight.
	resp, data = h.do("POST", "/session/"+root.ID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "concurrent prompt"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("concurrent prompt_async status %d: %s", resp.StatusCode, data)
	}
	var body struct {
		Status string `json:"status"`
	}
	mustUnmarshal(t, data, &body)
	if body.Status != "queued" {
		t.Fatalf("concurrent prompt_async status field = %q, want %q (never a second concurrent turn)", body.Status, "queued")
	}

	resumeBlocker.releaseAll()
	// The queued prompt must eventually run too (not lost) — the session
	// keeps cycling and its message count grows past the resume alone.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, data := h.do("GET", "/session/"+root.ID+"/message", nil)
		if resp.StatusCode == 200 && strings.Contains(string(data), "concurrent prompt") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("queued prompt never appears to have been delivered")
}

// TestCancelTreeAbortsRootInFlightTurn proves cancel_tree stops a ROOT's
// in-flight turn, not merely marks it canceled while the turn keeps
// running underneath — a live review finding: SessionManager.Cancel only
// ever cancels node.ctx, which a server-driven root turn (claimForPrompt/
// runPrompt) does not use.
func TestCancelTreeAbortsRootInFlightTurn(t *testing.T) {
	rootBlocker := newBlockingProvider("rootblock")
	t.Cleanup(rootBlocker.releaseAll)
	h := multiProviderHarness(t, message.ModelRef{Provider: "rootblock", Model: "m1"}, nil, rootBlocker)

	resp, data := h.do("POST", "/session", map[string]string{"model": "rootblock/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session/"+root.ID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	<-rootBlocker.started
	waitForLineageStatus(t, h, root.ID, "running", 2*time.Second)

	resp, data = h.do("DELETE", "/session/"+root.ID+"/cancel_tree", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("cancel_tree status %d: %s", resp.StatusCode, data)
	}

	// The turn must actually stop (the blocked provider call unblocks via
	// context cancellation) — lineage settles canceled promptly, not only
	// after rootBlocker is eventually released by this test's own cleanup.
	waitForLineageStatus(t, h, root.ID, "canceled", 2*time.Second)
}

// gatedProvider blocks in Stream until proceed is closed (or ctx ends) —
// unlike blockingProvider, it never signals "started" and is meant to be
// released explicitly by the test, from a precise point in a scripted
// sequence, not polled for.
type gatedProvider struct {
	name    string
	proceed chan struct{}
}

func (p *gatedProvider) Name() string { return p.name }

func (p *gatedProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	select {
	case <-p.proceed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &scriptedStream{events: asstTurn("first turn done")}, nil
}

// TestSelfResumeDoesNotRaceRunSlotRelease is the server-level regression
// test for the exact race a live review reproduced: a notification
// arriving for a root during its own final model call (too late for that
// turn's own checkout, engine.go's streamTurn) must trigger a self-resume
// that actually runs, not strand the node at StatusRunning forever
// because it raced runPrompt's own freeRunSlotAndEmitIdle.
//
// The race is reproduced with only real production code paths: the
// root's OWN turn is held open on a gated provider; while it's blocked
// (its own checkout has already run), a real child is spawned directly
// via SessionManager.Spawn (the same primitive the `task` tool and
// session.create use) and run to completion — enqueuing its notification
// on the root through the ordinary finalizeTurn path. Only THEN is the
// root's blocked turn released, so its notification genuinely arrived too
// late for that turn's own request.
func TestSelfResumeDoesNotRaceRunSlotRelease(t *testing.T) {
	gated := &gatedProvider{name: "gated", proceed: make(chan struct{})}
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("child done")}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "gated", Model: "m1"}, nil, gated, childProv)

	resp, data := h.do("POST", "/session", map[string]string{"model": "gated/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session/"+root.ID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	waitForLineageStatus(t, h, root.ID, "running", 2*time.Second)

	childID, err := h.srv.SessionManager().Spawn(engine.SpawnOptions{
		ParentID: root.ID,
		Prompt:   "go",
		Model:    message.ModelRef{Provider: "child", Model: "m1"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForLineageStatus(t, h, childID, "done", 2*time.Second)

	// The root's own turn is STILL blocked here — its checkout already
	// ran before the child even existed, so the notification just
	// enqueued on it is unambiguously "too late for this turn's own
	// request." Release it now.
	close(gated.proceed)

	// If the race is present, the node wedges at "running" forever
	// (nothing ever calls ReportTurnEnd for the stranded resume attempt).
	// If fixed, it cycles through the self-resume and settles idle.
	waitForLineageStatus(t, h, root.ID, "idle", 2*time.Second)

	resp, data = h.do("GET", "/session/"+root.ID+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
	}
	// The gated provider's scripted reply is the same canned text for
	// every call, so it never echoes the child's result — the proof of
	// delivery is the synthetic resume trigger message itself (the
	// EngineContext part carrying the child's actual result is never
	// persisted to history by design — see message.EngineContext's doc
	// comment — so this is the durable signal a resume genuinely ran).
	if !strings.Contains(string(data), "A background task you started has finished") {
		t.Errorf("self-resume never ran (node likely wedged at running instead): %s", data)
	}
}
