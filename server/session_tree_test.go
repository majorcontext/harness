package server

import (
	"context"
	"net/http"
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

// TestSessionSendToRootWithStrandedQueueIsNotLost is the regression test
// for a review finding: runOrQueueText's idle-with-non-empty-queue branch
// used to dispatch the queue's existing head without ever enqueuing THIS
// call's own text, silently dropping a session.send message any time the
// root's durable queue was already non-empty (a restart refold or a
// drain-gap strand — see TestQueueRestartRefoldNoAutoDispatch, whose
// direct-EnqueuePrompt technique this test reuses to arrange that state)
// while the response still claimed 202 "sent". Both turns — the
// pre-existing head, then this call's own text — must run, in FIFO order,
// with nothing dropped.
func TestSessionSendToRootWithStrandedQueueIsNotLost(t *testing.T) {
	prov := &scriptedProvider{name: "root", turns: [][]provider.Event{
		asstTurn("head reply"),
		asstTurn("sent reply"),
	}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil, prov)

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	// Strand a queued prompt on the idle root — the same shape a restart
	// refold or a drain-gap leaves behind.
	h.srv.mu.Lock()
	st := h.srv.sessions[root.ID]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatal("root not resident right after creation")
	}
	if _, err := st.sess.EnqueuePrompt("stranded head"); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	resp, data = h.do("POST", "/session/"+root.ID+"/send", map[string]string{"text": "my message"})
	if resp.StatusCode != 202 {
		t.Fatalf("send status %d: %s", resp.StatusCode, data)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, data := h.do("GET", "/session/"+root.ID+"/message", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
		}
		if strings.Contains(string(data), "head reply") && strings.Contains(string(data), "sent reply") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("both turns never completed (session.send text was lost): %s", data)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSessionSendToBusyRootIsQueuedNotLost is the regression test for a
// review finding distinct from TestSessionSendToRootWithStrandedQueueIsNotLost:
// runOrQueueText's early `return code != http.StatusNotFound` dropped
// session.send's text unconditionally whenever the root was simply BUSY
// (an ordinary claimForPrompt 409 — no pre-existing queue involved at
// all), not just in the already-fixed idle-with-non-empty-queue shape.
// sendTextToRoot's dedicated busy-path handling (mirroring
// enqueueOrDispatch) must durably enqueue instead. Also proves
// session.send's response is now honest about "queued" vs "sent",
// matching prompt_async's own status vocabulary, rather than always
// claiming "sent".
func TestSessionSendToBusyRootIsQueuedNotLost(t *testing.T) {
	blocker := newBlockingProvider("root")
	t.Cleanup(blocker.releaseAll)
	h := newHarness(t, blocker)
	id := h.createSession("root/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-blocker.started

	resp, data = h.do("POST", "/session/"+id+"/send", map[string]string{"text": "while busy"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("send-while-busy status %d: %s", resp.StatusCode, data)
	}
	var sendResp struct {
		Status string `json:"status"`
		Queued int    `json:"queued"`
	}
	mustUnmarshal(t, data, &sendResp)
	if sendResp.Status != "queued" || sendResp.Queued != 1 {
		t.Fatalf("send-while-busy response = %+v, want status=queued queued=1", sendResp)
	}

	blocker.releaseAll()

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, data := h.do("GET", "/session/"+id+"/message", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
		}
		if strings.Contains(string(data), "while busy") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued send-while-busy text never ran (was it lost?): %s", data)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSessionSendToBusyRootEvictedInGapIsRetryable409 is the regression
// test for a review finding: sendTextToRoot's busy branch, when the busy
// occupant is evicted from residency in the gap between claimForPrompt's
// failed claim and residentSession's own lookup, returned "queued"
// (success) without ever having durably enqueued text — permanently
// losing it while telling the caller it was accepted (a 202 the caller
// has no reason to retry). enqueueOrDispatch's own identical race
// (handlers.go) returns a retryable 409 for exactly this reason; this
// proves sendTextToRoot now does too.
func TestSessionSendToBusyRootEvictedInGapIsRetryable409(t *testing.T) {
	blocker := newBlockingProvider("root")
	t.Cleanup(blocker.releaseAll)
	h := newHarness(t, blocker)
	id := h.createSession("root/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-blocker.started

	// Force the exact gap the race lives in: by the time
	// sendTextToRoot's busy branch calls residentSession, the occupant
	// is gone from s.sessions — mirrors the real cause (a concurrent
	// claim evicting it once MaxResident is exceeded) without needing to
	// orchestrate one, exactly like this package's other *Race test-only
	// seams (queueDispatchRace, queueDeleteRace).
	h.srv.sendBusyEvictRace = func() {
		h.srv.mu.Lock()
		delete(h.srv.sessions, id)
		h.srv.mu.Unlock()
	}

	resp, data = h.do("POST", "/session/"+id+"/send", map[string]string{"text": "while busy, evicted in the gap"})
	if resp.StatusCode != 409 {
		t.Fatalf("send status %d, want 409 (retryable): %s", resp.StatusCode, data)
	}
}

// TestAbortStopsRunningManagedChild is the regression test for a review
// finding: POST /session/{childID}/abort was a silent no-op — a running
// child's turn runs on its SessionManager node.ctx (Spawn), never in
// s.sessions, so handleAbort's st == nil, cancel == nil, and it fell
// through to 204 having canceled nothing, misleading a caller into
// believing the child stopped while it ran to completion untouched.
// Proves abort now actually stops it.
func TestAbortStopsRunningManagedChild(t *testing.T) {
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
		"parent_id": root.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "blocker/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)

	waitForLineageStatus(t, h, child.ID, "running", 2*time.Second)

	resp, data = h.do("POST", "/session/"+child.ID+"/abort", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("abort status %d, want 204: %s", resp.StatusCode, data)
	}

	waitForLineageStatus(t, h, child.ID, "canceled", 2*time.Second)
}

// TestSpawnResponseReportsBusyNotIdle is the regression test for a
// review finding: the 201 body from session.create's parent_id form
// hard-coded top-level status/state "idle" even though Spawn always
// sets the new child StatusRunning before returning (a spawned child is
// handed work immediately) — self-inconsistent with the SAME response's
// own lineage block, which correctly reported "running" beside it.
func TestSpawnResponseReportsBusyNotIdle(t *testing.T) {
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
		"parent_id": root.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "blocker/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		Status  string         `json:"status"`
		State   string         `json:"state"`
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &child)
	if child.Status != "busy" {
		t.Errorf("top-level status = %q, want %q", child.Status, "busy")
	}
	if child.Lineage["status"] != "running" {
		t.Errorf("lineage.status = %v, want %q", child.Lineage["status"], "running")
	}
}

// TestAbortDiffersFromCancelTreeOnGrandchildOutcome is the regression
// test for a review finding: an earlier revision of handleAbort's child
// fallback called sessMgr.Cancel — the SAME full-subtree cascade
// cancel_tree uses — making abort indistinguishable from cancel_tree for
// a child with its own running descendants: every one of them got
// explicitly marked StatusCanceled. abort is now sessMgr.AbortTurn,
// which only ever explicitly cancels id itself; an actually-running
// descendant's turn is STILL interrupted (context derivation makes that
// unavoidable — a grandchild's ctx is context.WithCancel(child.ctx)) but
// reaches a DIFFERENT terminal state through the ordinary finalizeTurn
// path: StatusFailed (failReason "canceled"), not the explicit
// StatusCanceled a real cancel_tree would give it. This proves both
// halves of that distinction: the directly-aborted child ends up
// canceled (clean, matches user intuition), its caught-in-the-crossfire
// grandchild ends up failed (not canceled) — genuinely different
// operations, not the same one under two names.
func TestAbortDiffersFromCancelTreeOnGrandchildOutcome(t *testing.T) {
	blockerMid := newBlockingProvider("mid")
	blockerGrand := newBlockingProvider("grand")
	t.Cleanup(blockerMid.releaseAll)
	t.Cleanup(blockerGrand.releaseAll)
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, blockerMid, blockerGrand)

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "mid/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn mid status %d: %s", resp.StatusCode, data)
	}
	var mid struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &mid)
	waitForLineageStatus(t, h, mid.ID, "running", 2*time.Second)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": mid.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go deeper", "model": "grand/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn grand status %d: %s", resp.StatusCode, data)
	}
	var grand struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &grand)
	waitForLineageStatus(t, h, grand.ID, "running", 2*time.Second)

	resp, data = h.do("POST", "/session/"+mid.ID+"/abort", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("abort status %d, want 204: %s", resp.StatusCode, data)
	}

	waitForLineageStatus(t, h, mid.ID, "canceled", 2*time.Second)
	waitForLineageStatus(t, h, grand.ID, "failed", 2*time.Second)
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

// TestSessionSendToBusyChildIs409NotLost is the regression test for a
// review finding: handleSessionSend's child branch fired
// SessionManager.Send in a background goroutine and discarded its error
// unconditionally. A child has no prompt queue (unlike a root), so a Send
// against an already-running child returned ErrSessionBusy with nowhere
// to defer to — silently dropping the message while the caller still got
// 202 "sent". This proves session.send now refuses up front with 409
// instead.
// TestGenericTurnRoutesRejectManagedChild is the regression test for a
// BLOCKER: SessionManager is documented as a child's SOLE scheduler, but
// the generic per-{id} routes (prompt_async, goal, enqueue, compact,
// model, thinking) had no notion of that at all — each cold-loads its
// own *engine.Session for an id not currently server-resident (every
// SessionManager child, always) and would drive a turn or persist a
// durable record on it CONCURRENTLY with the child's own Spawn-driven
// turn on a DIFFERENT object for the SAME on-disk log. Proves each
// route now refuses a managed child with 409 instead.
func TestGenericTurnRoutesRejectManagedChild(t *testing.T) {
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("child done")}})

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)
	waitForLineageStatus(t, h, child.ID, "done", 2*time.Second)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"prompt_async", "POST", "/session/" + child.ID + "/prompt_async", map[string]any{"parts": []map[string]string{{"type": "text", "text": "hi"}}}},
		{"goal", "POST", "/session/" + child.ID + "/goal", map[string]string{"condition": "done"}},
		{"enqueue", "POST", "/session/" + child.ID + "/enqueue", map[string]any{"parts": []map[string]string{{"type": "text", "text": "hi"}}, "seq": 1}},
		{"compact", "POST", "/session/" + child.ID + "/compact", map[string]any{}},
		{"model", "POST", "/session/" + child.ID + "/model", map[string]string{"model": "root/m1"}},
		{"thinking", "POST", "/session/" + child.ID + "/thinking", map[string]string{"effort": "high"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := h.do(tc.method, tc.path, tc.body)
			if resp.StatusCode != 409 {
				t.Errorf("%s %s status = %d, want 409: %s", tc.method, tc.path, resp.StatusCode, data)
			}
		})
	}
}

func TestSessionSendToBusyChildIs409NotLost(t *testing.T) {
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

	resp, data = h.do("POST", "/session/"+child.ID+"/send", map[string]string{"text": "follow-up while busy"})
	if resp.StatusCode != 409 {
		t.Fatalf("send-to-busy-child status %d, want 409: %s", resp.StatusCode, data)
	}
}

// TestSessionSendToDoneChildAtConcurrencyCapIs409NotLost is the
// regression test for a review finding distinct from
// TestSessionSendToBusyChildIs409NotLost: an earlier fix pre-checked
// ONLY info.Status == StatusRunning before firing SessionManager.Send,
// missing the equally real, equally deterministic ErrConcurrencyLimit
// case entirely — a DONE child (session.send's own contract explicitly
// permits messaging one) whose tree has since filled its concurrency cap
// with OTHER running siblings. That is not a race (info.Status ==
// StatusRunning would never have caught it, since the target itself
// isn't running at all): it is Send's ordinary, expected admission
// failure whenever the tree is already busy elsewhere.
func TestSessionSendToDoneChildAtConcurrencyCapIs409NotLost(t *testing.T) {
	blockerB := newBlockingProvider("blockerB")
	blockerC := newBlockingProvider("blockerC")
	t.Cleanup(blockerB.releaseAll)
	t.Cleanup(blockerC.releaseAll)
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"},
		func(o *Options) { o.SessionManager = engine.NewSessionManager(context.Background(), 0, 2) }, // concurrency cap 2
		&scriptedProvider{name: "root"},
		&scriptedProvider{name: "childA", turns: [][]provider.Event{asstTurn("A done")}},
		blockerB, blockerC)

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "childA/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn childA status %d: %s", resp.StatusCode, data)
	}
	var childA struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &childA)
	waitForLineageStatus(t, h, childA.ID, "done", 2*time.Second)

	for _, model := range []string{"blockerB/m1", "blockerC/m1"} {
		resp, data := h.do("POST", "/session", map[string]string{
			"parent_id": root.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": model,
		})
		if resp.StatusCode != 201 {
			t.Fatalf("spawn %s status %d: %s", model, resp.StatusCode, data)
		}
		var child struct {
			ID string `json:"id"`
		}
		mustUnmarshal(t, data, &child)
		waitForLineageStatus(t, h, child.ID, "running", 2*time.Second)
	}

	// The tree's concurrency cap (2) is now fully occupied by the two
	// blocking children. childA is DONE, not running — a status-only
	// pre-check would wrongly let this through.
	resp, data = h.do("POST", "/session/"+childA.ID+"/send", map[string]string{"text": "follow-up at cap"})
	if resp.StatusCode != 409 {
		t.Fatalf("send-to-done-child-at-cap status %d, want 409: %s", resp.StatusCode, data)
	}
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
