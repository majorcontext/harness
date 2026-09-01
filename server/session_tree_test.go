package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	return multiProviderHarnessInDir(t, t.TempDir(), model, mutate, providers...)
}

// multiProviderHarnessInDir is multiProviderHarness with an explicit,
// caller-supplied SessionDir instead of a fresh t.TempDir() per call — so
// two independent harnesses (two independent *Server, each its own fresh
// engine.SessionManager) can share the SAME on-disk session storage,
// simulating a real process restart at the test level: the second
// harness's SessionManager starts with an EMPTY m.nodes, exactly like a
// freshly started `harness serve` process would, while the first
// harness's on-disk state (including anything Persist wrote) is still
// there for it to cold-load.
func multiProviderHarnessInDir(t *testing.T, dir string, model message.ModelRef, mutate func(*Options), providers ...provider.Provider) *harness {
	t.Helper()
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

// waitForLineageStatus blocks until id's lineage.status, read over the
// wire from GET /session/{id}, reads want.
//
// The assertion still goes through the production HTTP surface, but the
// wait between reads blocks on engine.SessionManager.Changed — the
// manager's own "a node's state settled" signal, which is where
// lineage.status comes from. Nothing samples on an interval, so nothing
// guesses how long a transition takes; timeout is a failure bound only.
// Changed is armed BEFORE each read, so a transition landing between the
// read and the wait is still delivered.
func waitForLineageStatus(t *testing.T, h *harness, id, want string, timeout time.Duration) map[string]any {
	t.Helper()
	mgr := h.srv.SessionManager()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		changed := mgr.Changed()
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
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("GET /session/%s: lineage.status = %v after %s, want %q", id, got.Lineage["status"], timeout, want)
		}
	}
}

// waitForMessageText blocks on the session's real SSE stream until a
// durable message event whose text contains want arrives. The caller opens
// the stream BEFORE the action that should produce the message, so no event
// can be missed between the action and the wait.
//
// This is the right wait whenever the turn that produces the message is
// started by the engine itself, asynchronously — a task-completion resume,
// for instance. GET /session/{id}/wait?until=idle cannot serve there: the
// session is still idle at the moment the caller starts waiting, so the
// long-poll returns at once, before the resume turn it means to wait for
// has even claimed the run slot.
func waitForMessageText(t *testing.T, stream *sseStream, want string, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		var it sseItem
		select {
		case v, ok := <-stream.items:
			if !ok {
				t.Fatalf("sse stream closed before a message containing %q arrived", want)
			}
			it = v
		case <-timer.C:
			t.Fatalf("no message containing %q arrived within %s", want, timeout)
		}
		if it.heartbeat || it.ev.Type != "message" || it.ev.Message == nil {
			continue
		}
		for _, part := range it.ev.Message.Parts {
			if txt, ok := part.(*message.Text); ok && strings.Contains(txt.Text, want) {
				return
			}
		}
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

// TestSessionCreateWithParentIDFiresOnTaskEvent is the regression test
// for a follow-up finding ("metrics"): handleSpawnChild now reports each
// spawn outcome to Options.OnTaskEvent — "spawned" on success,
// "depth_refused"/"concurrency_refused"/"budget_refused" mirroring the
// matching engine sentinel. Proves both the success case and the
// depth-refused case (the cheapest of the three limits to trigger in a
// unit test — a depth-0 SessionManager).
func TestSessionCreateWithParentIDFiresOnTaskEvent(t *testing.T) {
	var mu sync.Mutex
	var events []string
	onTaskEvent := func(event, parentID, childID string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
		if parentID == "" {
			t.Error("OnTaskEvent called with empty parentID")
		}
		if event == "spawned" && childID == "" {
			t.Error("OnTaskEvent(\"spawned\", ...) called with empty childID")
		}
	}
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("done")}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, func(o *Options) {
		o.OnTaskEvent = onTaskEvent
	}, &scriptedProvider{name: "root"}, childProv)

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentExplore, "prompt": "go", "model": "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)

	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "spawned" {
		t.Errorf("events = %v, want [spawned]", got)
	}

	// Spawn returns before the child's turn runs. Wait for it to settle:
	// that turn creates and writes the child's durable files, and a test
	// that returns first leaves those writes racing t.TempDir's cleanup,
	// which then fails with "directory not empty". waitForLineageStatus
	// blocks on SessionManager.Changed, the production seam, not a sleep.
	waitForLineageStatus(t, h, child.ID, "done", 5*time.Second)
}

// TestReportTaskEventClassifiesEachSentinel proves reportTaskEvent's
// error-to-event-string mapping directly, covering the three refusal
// cases TestSessionCreateWithParentIDFiresOnTaskEvent's HTTP round trip
// above does not exercise (each requires a limit genuinely at capacity,
// awkward to set up over HTTP — this is the same classification logic,
// tested directly).
func TestReportTaskEventClassifiesEachSentinel(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "spawned"},
		{engine.ErrDepthLimit, "depth_refused"},
		{engine.ErrConcurrencyLimit, "concurrency_refused"},
		{engine.ErrBudgetExceeded, "budget_refused"},
	}
	for _, tc := range cases {
		var got string
		h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, func(o *Options) {
			o.OnTaskEvent = func(event, parentID, childID string) { got = event }
		}, &scriptedProvider{name: "root"})
		h.srv.reportTaskEvent("ses_parent", "ses_child", tc.err)
		if got != tc.want {
			t.Errorf("reportTaskEvent(err=%v) event = %q, want %q", tc.err, got, tc.want)
		}
	}
	// ErrUnknownSession and any other unclassified error: no event at all.
	var called bool
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, func(o *Options) {
		o.OnTaskEvent = func(event, parentID, childID string) { called = true }
	}, &scriptedProvider{name: "root"})
	h.srv.reportTaskEvent("ses_parent", "", engine.ErrUnknownSession)
	if called {
		t.Error("reportTaskEvent(ErrUnknownSession) fired OnTaskEvent, want no call")
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

// TestColdChildHasDurableLineage is the regression test for a follow-up
// finding: GET /session/{id}'s lineage block used to require this
// process's SessionManager to have the session adopted in memory
// (sessMgr.Info succeeding) — a child Reaped, or simply never touched
// since a fresh process started (simulated here via a SECOND, independent
// harness sharing the first's on-disk session storage — see
// multiProviderHarnessInDir), reported NO lineage at all, even though its
// parent id and agent type are fully durable on disk (Config.
// TaskParentID/TaskAgentType, restored by LoadSession unconditionally).
// Proves the cold-fallback branch in lineageJSONFor surfaces parent_id
// and agent_type without requiring any write (a prompt/send call) to
// force a reload first.
//
// Also covers two later review findings on the same cold-fallback branch,
// both concluding "no durable source" for Depth and Children when there
// actually IS one — Config.TaskDepth and Session.SpawnedChildIDs(),
// exactly as durable and unconditional as TaskParentID/TaskAgentType
// above, both restored by LoadSession without any SessionManager adoption
// needed. An earlier revision of this fix left Depth omitted and Children
// an honest-but-needlessly-conservative JSON null on the cold path,
// reasoning neither was recoverable without a live parent chain — true for
// Depth only until Config.TaskDepth started recording the real value at
// Spawn time, and never actually true for Children, which the cold
// branch's own already-loaded sess had the complete durable answer for
// the whole time. This test proves BOTH are now the real, durably-known
// values on the cold path: depth 1 (child is root's direct child) and
// children [grandchild.ID] (child's own SpawnedChildIDs), not "unknown".
func TestColdChildHasDurableLineage(t *testing.T) {
	dir := t.TempDir()
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("the answer is 42")}}
	grandchildProv := &scriptedProvider{name: "grandchild", turns: [][]provider.Event{asstTurn("the deeper answer is 43")}}
	h1 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv, grandchildProv)

	resp, data := h1.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h1.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentExplore, "prompt": "find the answer", "model": "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)
	waitForLineageStatus(t, h1, child.ID, "done", 2*time.Second)

	// Give the child its own grandchild — a real, live subtree the child
	// (a mid-tree parent from here on) genuinely has, on disk, once h1
	// itself goes cold below.
	resp, data = h1.do("POST", "/session", map[string]string{
		"parent_id": child.ID, "agent": engine.AgentExplore, "prompt": "go deeper", "model": "grandchild/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn grandchild status %d: %s", resp.StatusCode, data)
	}
	var grandchild struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &grandchild)
	waitForLineageStatus(t, h1, grandchild.ID, "done", 2*time.Second)

	// A SECOND, independent harness against the SAME dir: a fresh
	// SessionManager that has never seen child.ID — the exact "cold"
	// condition (Reap, or a real process restart) this fix targets.
	// Deliberately never sends the child a prompt: that would trigger
	// ReportTurnStart's own adopt-on-first-sight reload and mask the bug
	// this test exists to catch.
	h2 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv, grandchildProv)
	resp, data = h2.do("GET", "/session/"+child.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("cold GET child status %d: %s", resp.StatusCode, data)
	}
	var cold struct {
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &cold)
	if cold.Lineage == nil {
		t.Fatal("cold GET has no lineage at all — durable fallback did not fire")
	}
	if cold.Lineage["parent_id"] != root.ID {
		t.Errorf("cold lineage.parent_id = %v, want %q", cold.Lineage["parent_id"], root.ID)
	}
	if cold.Lineage["agent_type"] != engine.AgentExplore {
		t.Errorf("cold lineage.agent_type = %v, want %q", cold.Lineage["agent_type"], engine.AgentExplore)
	}
	// Status still has no durable source at all and must stay omitted.
	if _, ok := cold.Lineage["status"]; ok {
		t.Errorf("cold lineage.status = %v, want omitted (no durable source)", cold.Lineage["status"])
	}
	// Depth and Children, unlike Status, DO have a durable source — Config.
	// TaskDepth and Session.SpawnedChildIDs(), both restored by LoadSession
	// unconditionally, no SessionManager adoption needed (see
	// lineageJSONFor's own doc comment) — so the cold path now reports the
	// real values instead of guessing "unknown". child is root's direct
	// child, so its true depth is 1.
	if cold.Lineage["depth"] != float64(1) {
		t.Errorf("cold lineage.depth = %v, want 1 (durable TaskDepth)", cold.Lineage["depth"])
	}
	// children has no omitempty (see its own doc comment, handlers.go) —
	// present, and now the REAL durable list (SpawnedChildIDs), not an
	// affirmative-but-wrong []: the child's own persisted log durably
	// records which children IT spawned. A NON-empty SpawnedChildIDs() is
	// trustworthy on the cold path regardless of log vintage — see
	// TestColdChildlessLineageChildrenIsUnknownNotZero for the OTHER
	// half, where an EMPTY cold-path SpawnedChildIDs() is deliberately
	// reported as unknown (null), not confirmed zero.
	got, ok := cold.Lineage["children"].([]any)
	if !ok {
		t.Fatalf("cold lineage.children = %v (%T), want a one-element array", cold.Lineage["children"], cold.Lineage["children"])
	}
	if len(got) != 1 || got[0] != grandchild.ID {
		t.Errorf("cold lineage.children = %v, want [%q]", got, grandchild.ID)
	}
}

// TestColdChildlessLineageChildrenIsUnknownNotZero is the regression test
// for a review finding: the cold-fallback branch used to run Children
// through childIDsUnion just like the warm branch, normalizing an empty
// sess.SpawnedChildIDs() to a confirmed "children":[]. But
// SpawnedChildIDs() is a complete answer only for a log written after
// recTaskSpawned records shipped — a legacy log that genuinely spawned
// children before that record existed has an empty SpawnedChildIDs()
// indistinguishable from a parent that truly never spawned anything, and
// unlike the warm branch, the cold branch has no live tree to cross-check
// an empty result against. So an empty cold-path SpawnedChildIDs() must
// report "children":null (genuinely unknown), matching how this exact
// branch already treats Status/Result/FailReason — not an affirmative,
// possibly-false "zero".
//
// This test's child is genuinely, unambiguously childless (never spawned
// anything, in a log written with the current field set) — so this is
// the conservative, over-cautious direction of the fix: even a case that
// COULD safely report "known: zero" now reports "unknown" instead, since
// the cold branch has no way to distinguish it from the legacy case
// above using only sess.SpawnedChildIDs(). That conservatism, not
// precision for this specific case, is the deliberate tradeoff.
func TestColdChildlessLineageChildrenIsUnknownNotZero(t *testing.T) {
	dir := t.TempDir()
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("the answer is 42")}}
	h1 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)

	resp, data := h1.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h1.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentExplore, "prompt": "find the answer", "model": "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)
	waitForLineageStatus(t, h1, child.ID, "done", 2*time.Second)
	// This child never spawns anything of its own (an "explore" leaf) —
	// SpawnedChildIDs() is genuinely, unambiguously empty.

	// A SECOND, independent harness against the SAME dir: a fresh
	// SessionManager that has never seen child.ID — the cold-fallback
	// condition, exactly like TestColdChildHasDurableLineage.
	h2 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)
	resp, data = h2.do("GET", "/session/"+child.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("cold GET child status %d: %s", resp.StatusCode, data)
	}
	var cold struct {
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &cold)
	if cold.Lineage == nil {
		t.Fatal("cold GET has no lineage at all — durable fallback did not fire")
	}
	v, ok := cold.Lineage["children"]
	if !ok {
		t.Fatal("cold lineage.children key missing entirely, want present as JSON null")
	}
	if v != nil {
		t.Errorf("cold lineage.children = %v, want null (unknown) — an empty SpawnedChildIDs() on the cold path cannot be told apart from a legacy pre-recTaskSpawned log that lost the record", v)
	}
}

// TestWarmOrphanChildLineageKeepsDurableParentID covers the WARM
// orphaned-parent shape (a review finding on the TaskDepth fix): a child
// adopted by adoptReloadedLocked while its parent is untracked gets its
// depth from durable TaskDepth, but the node's parentID stays empty (only
// the live-parent branch sets attachTo). lineageJSONFor's warm branch must
// then fall back to the durable TaskParentID. Without the fallback, the
// wire showed depth 1 with no parent_id — a shape lineageJSON.Depth's doc
// comment rules out ("depth 0 only ever occurs for a root"), and one that
// makes a tree-reconstructing client misfile a real child as a root.
//
// The orphan state is real: after a restart, ReportTurnStart's
// adopt-on-first-sight reload adopts a child whose parent nothing has
// touched yet. This test builds that state directly: spawn a child in one
// harness, then adopt only its reload into a second harness's fresh
// SessionManager via ReportTurnStart, and GET it there.
func TestWarmOrphanChildLineageKeepsDurableParentID(t *testing.T) {
	dir := t.TempDir()
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("the answer is 42")}}
	h1 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)

	resp, data := h1.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h1.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentExplore, "prompt": "find the answer", "model": "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)
	waitForLineageStatus(t, h1, child.ID, "done", 2*time.Second)

	// Fresh harness, same dir: an empty SessionManager, like a restarted
	// process. Adopt ONLY the child, the way ReportTurnStart's
	// adopt-on-first-sight path would — root stays untracked, so
	// adoptReloadedLocked takes its durable-TaskDepth branch.
	h2 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)
	reloaded, err := engine.LoadSession(engine.Config{SessionDir: dir, Model: message.ModelRef{Provider: "child", Model: "m1"}}, child.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	h2.srv.sessMgr.ReportTurnStart(reloaded)

	resp, data = h2.do("GET", "/session/"+child.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("warm-orphan GET child status %d: %s", resp.StatusCode, data)
	}
	var got struct {
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &got)
	if got.Lineage == nil {
		t.Fatal("warm-orphan GET has no lineage at all")
	}
	if got.Lineage["depth"] != float64(1) {
		t.Errorf("warm-orphan lineage.depth = %v, want 1 (durable TaskDepth)", got.Lineage["depth"])
	}
	if got.Lineage["parent_id"] != root.ID {
		t.Errorf("warm-orphan lineage.parent_id = %v, want %q (durable TaskParentID fallback)", got.Lineage["parent_id"], root.ID)
	}
}

// TestSentinelPoisonedChainWireDepthMatchesEnforcement is the adjudicated
// fix for a review finding: an earlier revision of lineageJSONFor
// independently re-preferred sess.TaskDepth() over info.Depth on the wire
// — a SECOND derivation of "this session's depth" that could disagree
// with the ENFORCEMENT depth (info.Depth, the exact value TaskToolAllowed
// gates this session's own `task` tool against) whenever a poisoned
// ancestor's refusal-sentinel depth propagates forward into a legacy
// descendant with no durable TaskDepth of its own. lineage.depth must
// report exactly what is enforced, not a value a caller could act on and
// be wrong about. Proves the wire and enforcement now agree by
// construction (lineageJSONFor reports info.Depth verbatim): whatever
// depth GET /session/{id} shows is exactly what SessionManager already
// decided.
//
// Builds the poisoned chain directly: "mid" is legacy (no durable
// TaskDepth) with its own parent untracked, so adoptReloadedLocked gives
// it the m.maxDepth refusal sentinel (the harness default, 3 — see
// engine.DefaultMaxTaskDepth). "child" is ALSO legacy, adopted under mid
// (now tracked) — its enforcement depth becomes sentinel+1 = 4, one past
// the configured limit.
func TestSentinelPoisonedChainWireDepthMatchesEnforcement(t *testing.T) {
	dir := t.TempDir()
	h := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"})

	midCfg := engine.Config{SessionDir: dir, Model: message.ModelRef{Provider: "root", Model: "m1"}, TaskParentID: "ses_0000000000000099"}
	mid := engine.NewSession(midCfg)
	h.srv.sessMgr.ReportTurnStart(mid)
	midInfo, ok := h.srv.sessMgr.Info(mid.ID)
	if !ok || midInfo.Depth != 3 {
		t.Fatalf("mid enforcement depth = %v (ok=%v), want 3 (the sentinel — test setup invalid)", midInfo.Depth, ok)
	}

	childCfg := engine.Config{SessionDir: dir, Model: message.ModelRef{Provider: "root", Model: "m1"}, TaskParentID: mid.ID}
	child := engine.NewSession(childCfg)
	h.srv.sessMgr.ReportTurnStart(child)
	childInfo, ok := h.srv.sessMgr.Info(child.ID)
	if !ok || childInfo.Depth != 4 {
		t.Fatalf("child enforcement depth = %v (ok=%v), want 4 (sentinel+1 — test setup invalid)", childInfo.Depth, ok)
	}

	resp, data := h.do("GET", "/session/"+child.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET child status %d: %s", resp.StatusCode, data)
	}
	var got struct {
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &got)
	if got.Lineage == nil {
		t.Fatal("no lineage in response")
	}
	if got.Lineage["depth"] != float64(childInfo.Depth) {
		t.Errorf("wire lineage.depth = %v, want %v (must match enforcement's info.Depth exactly, not a separately-derived value)", got.Lineage["depth"], childInfo.Depth)
	}
}

// TestWarmChildlessLineageHasExplicitEmptyChildren is the regression test
// for the OTHER half of Children's own fix (see TestColdChildHasDurableLineage's
// doc comment for the full history): a WARM, genuinely childless node
// must serialize "children":[] — present and explicitly empty, never
// omitted (which would be indistinguishable from the cold-fallback
// branch's own honest "unknown" null) and never simply absent from the
// map. Uses a plain root with no children spawned: sessMgr.Info succeeds
// for a root exactly like it does for a child, so lineageJSONFor's warm
// branch is exercised the same way.
func TestWarmChildlessLineageHasExplicitEmptyChildren(t *testing.T) {
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil, &scriptedProvider{name: "root"})
	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("GET", "/session/"+root.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get root status %d: %s", resp.StatusCode, data)
	}
	var got struct {
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &got)
	v, ok := got.Lineage["children"]
	if !ok {
		t.Fatal("warm childless lineage.children key missing entirely, want present as []")
	}
	list, isList := v.([]any)
	if !isList {
		t.Fatalf("warm childless lineage.children = %#v (type %T), want an empty JSON array, not null or omitted", v, v)
	}
	if len(list) != 0 {
		t.Errorf("warm childless lineage.children = %v, want empty", list)
	}
}

// TestSessionCreateWithParentIDUnconfiguredModelIs400 is the regression
// test for a live review finding: handleSpawnChild parsed a model override
// but never validated its provider was configured, unlike the `task` tool's
// identical check (runTaskTool) — an override naming a provider nothing
// registers used to sail through Spawn, consuming a concurrency slot and a
// session log, only to fail later at the child's own first turn. Proves it
// is now rejected synchronously, before anything is spawned.
func TestSessionCreateWithParentIDUnconfiguredModelIs400(t *testing.T) {
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
		"agent":     engine.AgentExplore,
		"prompt":    "go",
		"model":     "totally-unconfigured-provider/some-model",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("unconfigured provider status = %d, want 400: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "totally-unconfigured-provider") {
		t.Errorf("error body = %s, want it to name the unconfigured provider", data)
	}

	// No child should have been spawned: root has no children yet.
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
	if len(rootInfo.Lineage.Children) != 0 {
		t.Errorf("root lineage.children = %v, want none — a child was spawned despite the unconfigured provider", rootInfo.Lineage.Children)
	}
}

// TestSessionCreateWithParentIDUnconfiguredDefinitionModelIs400 is the
// regression test for a second live review finding on the same fix: the
// first pass at handleSpawnChild's provider validation only covered the
// REQUEST BODY's model override, missing that def.Model — a custom
// .agents/*.md definition's own "model:" frontmatter — sails through
// exactly the same way when the request supplies no override at all.
func TestSessionCreateWithParentIDUnconfiguredDefinitionModelIs400(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defContent := "---\n" +
		"name: custom\n" +
		"description: A custom agent whose own definition names an unconfigured provider\n" +
		"model: totally-unconfigured-provider/some-model\n" +
		"---\n" +
		"A custom agent.\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "custom.md"), []byte(defContent), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newWorkdirHarness(t, &scriptedProvider{name: "root"}, []string{root})

	resp, data := h.do("POST", "/session", map[string]any{"model": "root/m1", "workdir": root})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var rootSess struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &rootSess)

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": rootSess.ID,
		"agent":     "custom",
		"prompt":    "go",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("unconfigured definition model status = %d, want 400: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "totally-unconfigured-provider") {
		t.Errorf("error body = %s, want it to name the unconfigured provider", data)
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

	// POST /send claims the run slot synchronously before it answers 202,
	// so the session already reads busy here: waiting for idle through the
	// production long-poll cannot return on a pre-turn idle. It also
	// covers the queue drain (waitSnapshot folds in queueDrainPending), so
	// idle here means every turn this send caused has finished.
	h.waitIdle(root.ID)
	resp, data = h.do("GET", "/session/"+root.ID+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "hello back") {
		t.Fatalf("session never received the reply: %s", data)
	}
	// A session.send delivery is a genuine operator-authored message,
	// never the engine's own synthetic resume trigger — it must not carry
	// origin:engine (see sendTextToRoot's own doc comment and
	// message.Message.Origin's).
	if strings.Contains(string(data), `"origin":"engine"`) {
		t.Errorf("session.send message wrongly carries origin:engine: %s", data)
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
	if _, _, err := st.sess.EnqueuePrompt("stranded head", ""); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}

	resp, data = h.do("POST", "/session/"+root.ID+"/send", map[string]string{"text": "my message"})
	if resp.StatusCode != 202 {
		t.Fatalf("send status %d: %s", resp.StatusCode, data)
	}

	// until=idle spans BOTH turns: waitSnapshot folds in
	// queueDrainPending, so the head turn's own idle does not wake this
	// wait while the sent text is still queued behind it.
	h.waitIdle(root.ID)
	resp, data = h.do("GET", "/session/"+root.ID+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "head reply") || !strings.Contains(string(data), "sent reply") {
		t.Fatalf("both turns never completed (session.send text was lost): %s", data)
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

	h.waitIdle(id)
	resp, data = h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "while busy") {
		t.Fatalf("queued send-while-busy text never ran (was it lost?): %s", data)
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
	// Released explicitly at the end of the test body below, NOT relied
	// on solely via t.Cleanup: a child's Prompt call runs on
	// SessionManager's own node.ctx, entirely independent of the test
	// server's HTTP connection tracking, so nothing in multiProviderHarness's
	// own t.Cleanup(ts.Close) would ever wait for it — it can still be
	// mid-write to its session log file under t.TempDir() when TempDir's
	// OWN cleanup (also LIFO-ordered, also registered AFTER this one)
	// tries to remove the directory, intermittently failing with
	// "directory not empty" under load. A live full-suite run caught
	// this exact flake.
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
		ID      string         `json:"id"`
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

	blocker.releaseAll()
	waitForLineageStatus(t, h, child.ID, "done", 2*time.Second)
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

// TestAbortOfTerminalChildLeavesGrandchildAndSendUntouched is the
// regression test for a review finding: handleAbort routed to
// sessMgr.AbortTurn for ANY managed child regardless of its own status.
// AbortTurn cancels the node's context unconditionally, which (a) is
// pure collateral damage for a child that has ALREADY finished (its own
// context has nothing left to interrupt) — the review reproduced this
// exactly: aborting a done child killed its own still-running grandchild
// (spawned during an earlier, already-finished turn) — and (b)
// permanently breaks the done child's own later reachability, since a
// context.CancelFunc never re-arms: a legitimate follow-up session.send
// to it would instantly fail with a canceled context via mergeCancel.
// AbortTurn is now a no-op unless the target is CURRENTLY StatusRunning.
// This proves both halves: aborting an already-done child leaves its
// still-running grandchild alone, and the done child itself remains
// sendable afterward.
func TestAbortOfTerminalChildLeavesGrandchildAndSendUntouched(t *testing.T) {
	blockerGrand := newBlockingProvider("grand")
	t.Cleanup(blockerGrand.releaseAll)
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"},
		&scriptedProvider{name: "mid", turns: [][]provider.Event{asstTurn("mid done"), asstTurn("mid follow-up done")}},
		blockerGrand)

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
	waitForLineageStatus(t, h, mid.ID, "done", 2*time.Second)

	// A grandchild spawned from mid AFTER mid already finished — a real,
	// legitimate shape (a done parent can still have live descendants;
	// see cancelSubtreeLocked's own doc comment on that exact point).
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

	// mid was never running — must stay exactly as it was.
	resp, data = h.do("GET", "/session/"+mid.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get mid status %d: %s", resp.StatusCode, data)
	}
	var midInfo struct {
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &midInfo)
	if midInfo.Lineage["status"] != "done" {
		t.Errorf("mid lineage.status = %v, want %q (abort of a non-running node must be a no-op)", midInfo.Lineage["status"], "done")
	}

	// mid must still be sendable — its context was never canceled.
	resp, data = h.do("POST", "/session/"+mid.ID+"/send", map[string]string{"text": "follow-up"})
	if resp.StatusCode != 202 {
		t.Fatalf("send to mid after its own no-op abort status %d, want 202: %s", resp.StatusCode, data)
	}
	waitForLineageStatus(t, h, mid.ID, "done", 2*time.Second)

	// grand must be untouched collateral: its turn must still be able to
	// finish normally, as "done".
	//
	// Reading grand's status right here instead would prove nothing. A
	// wrongful cascade cancels grand's context synchronously inside the
	// abort call, but grand only LEAVES "running" later, asynchronously,
	// in its own goroutine — blockingStream returns on the canceled
	// context and finalizeTurn runs there. Nothing orders that goroutine
	// against this one: mid's follow-up turn and grand's finalize both
	// take m.mu, but two goroutines taking the same lock impose no
	// happens-before between them. So a status read could land first, see
	// "running", and pass while the regression it names was live.
	//
	// Release grand's provider and wait for its own terminal status
	// instead. That has a real edge with grand's goroutine, and it is a
	// positive assertion: an uncanceled grand runs its scripted turn to
	// "done", while a wrongfully canceled one ends "canceled" and this
	// wait fails naming the status it actually saw.
	blockerGrand.releaseAll()
	waitForLineageStatus(t, h, grand.ID, "done", 2*time.Second)
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

// TestWorkdirHeldResumeRefusalDoesNotPinRootRunning is the regression
// test for a review finding: triggerResumeLocked flips a root to
// StatusRunning BEFORE calling its ExternalRunner
// (resumeSessionForTaskNotification -> runOrQueueText ->
// claimForPrompt). An earlier revision of runOrQueueText treated ANY
// non-404 claim failure as handled=true, including a workdir-held 409 —
// where NO turn is running on the root at all (a DIFFERENT session
// entirely holds the shared workdir), so nothing would ever call
// ReportTurnEnd to release that speculative commitment. The root got
// permanently stuck StatusRunning: queue-or-resume dead for it, its
// pending notification never delivered, until an unrelated human prompt
// happened to drain it.
//
// Proven end-to-end at the wire level: root A holds the shared (default)
// workdir busy; a child spawned under idle root B completes and tries to
// resume B, refused by the workdir conflict. B must return to idle
// (not get stuck "running") — and once A frees the workdir, a SECOND
// child's completion must successfully trigger a real resume turn on B
// (observable as the synthetic resume-trigger user message reaching B's
// history), proving the first notification's refusal didn't strand
// queue-or-resume for B permanently.
func TestWorkdirHeldResumeRefusalDoesNotPinRootRunning(t *testing.T) {
	blockerA := newBlockingProvider("rootA")
	t.Cleanup(blockerA.releaseAll)
	h := multiProviderHarness(t, message.ModelRef{Provider: "rootB", Model: "m1"}, nil,
		blockerA, &scriptedProvider{name: "rootB"},
		&scriptedProvider{name: "child1", turns: [][]provider.Event{asstTurn("child1 done")}},
		&scriptedProvider{name: "child2", turns: [][]provider.Event{asstTurn("child2 done")}})

	// Both default to the process cwd — the same workdir, no explicit
	// override — exactly TestPromptSameWorkdirBusyRejected's own setup.
	resp, data := h.do("POST", "/session", map[string]string{"model": "rootA/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root A status %d: %s", resp.StatusCode, data)
	}
	var rootA struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &rootA)

	resp, data = h.do("POST", "/session", map[string]string{"model": "rootB/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root B status %d: %s", resp.StatusCode, data)
	}
	var rootB struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &rootB)

	// A claims and holds the shared workdir.
	resp, data = h.do("POST", "/session/"+rootA.ID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt A status %d: %s", resp.StatusCode, data)
	}
	<-blockerA.started

	// child1, spawned under idle B, completes fast and tries to resume
	// B — refused by A's workdir hold.
	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": rootB.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "child1/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child1 status %d: %s", resp.StatusCode, data)
	}
	var child1 struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child1)
	waitForLineageStatus(t, h, child1.ID, "done", 2*time.Second)

	// B must NOT be stuck "running" — the core assertion.
	waitForLineageStatus(t, h, rootB.ID, "idle", 2*time.Second)

	const resumeTriggerText = "A background task you started has finished. See the engine context below for its result, and continue accordingly."
	// Opened before child2 exists, so the resume turn's message event
	// cannot be missed between the spawn and the wait at the end.
	streamB := h.openSSE("?session="+rootB.ID, "")
	resp, data = h.do("GET", "/session/"+rootB.ID+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get B messages status %d: %s", resp.StatusCode, data)
	}
	if strings.Contains(string(data), resumeTriggerText) {
		t.Fatalf("B ran a resume turn despite the workdir conflict refusing it: %s", data)
	}

	// Free the workdir; child2's completion must find B idle (thanks to
	// the fix reverting child1's stuck attempt) and successfully trigger
	// a real resume turn this time.
	blockerA.releaseAll()
	waitForLineageStatus(t, h, rootA.ID, "idle", 2*time.Second) // a root's own status is never "done" — only a child's is

	resp, data = h.do("POST", "/session", map[string]string{
		"parent_id": rootB.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "child2/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child2 status %d: %s", resp.StatusCode, data)
	}
	var child2 struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child2)
	waitForLineageStatus(t, h, child2.ID, "done", 2*time.Second)

	// B's resume turn is started by the engine, asynchronously, once
	// child2 completes — B is still idle at this instant, so an
	// until=idle wait would return immediately and prove nothing. Block on
	// B's own SSE stream instead, opened before child2 was ever spawned.
	waitForMessageText(t, streamB, resumeTriggerText, 5*time.Second)
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

// TestSessionEndCascadesToChild is the regression test for a follow-up
// finding: plain DELETE /session/{id} (handleEnd) used to only ever touch
// server residency (s.sessions), never sessMgr, so ending a parent with a
// still-running child silently orphaned it — the child kept running to
// completion with no one left to ever check out its result, since its
// parent's own row was already gone. Mirrors TestCancelTreeCascadesToChild
// exactly, but drives plain DELETE (not /cancel_tree) — proves handleEnd
// itself now cascade-cancels live children.
func TestSessionEndCascadesToChild(t *testing.T) {
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

	resp, data = h.do("DELETE", "/session/"+root.ID, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("end status %d: %s", resp.StatusCode, data)
	}

	waitForLineageStatus(t, h, child.ID, "canceled", 2*time.Second)
}

// TestSessionEndForgetsRootFromSessionManager is the regression test for
// a follow-up finding: DELETE /session/{id} used to only ever touch
// server residency (s.sessions), never sessMgr — a root's sessionNode
// (and the *Session it pins: full message history, ctx) survived in
// sessMgr's m.nodes for the rest of the PROCESS's life even after its
// caller explicitly deleted it, since Reap's own documented contract
// never removes a root automatically. Proves handleEnd's new
// ForgetRoot call actually closes that leak for the common case: a
// plain, childless root.
func TestSessionEndForgetsRootFromSessionManager(t *testing.T) {
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"})

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	if _, ok := h.srv.SessionManager().Info(root.ID); !ok {
		t.Fatal("test setup: root not tracked by SessionManager before DELETE")
	}

	resp, data = h.do("DELETE", "/session/"+root.ID, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("end status %d: %s", resp.StatusCode, data)
	}

	if _, ok := h.srv.SessionManager().Info(root.ID); ok {
		t.Error("root still tracked by SessionManager after DELETE — leaked (Reap never removes a root automatically)")
	}
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

// TestGenericTurnRoutesRejectWarmOrphanChild is the regression test for a
// review finding: rejectManagedChildTurn used to key "is this a managed
// child" on info.ParentID != "", the LIVE tree pointer — but
// adoptReloadedLocked leaves info.ParentID EMPTY for a warm orphan (a
// genuine child adopted while its own parent was untracked at adopt time
// — see that method's own doc comment and
// TestWarmOrphanChildLineageKeepsDurableParentID). A warm orphan's
// durable TaskParentID is still set, and it is still very much a managed
// child SessionManager is the SOLE scheduler for, but the old check let
// it slip through this guard entirely — the exact concurrent-Session
// corruption (a SECOND *engine.Session cold-loaded over the same
// on-disk log, driven concurrently with the child's own object) this
// guard exists to prevent, reachable through precisely the child shape
// least equipped to survive it.
//
// Builds the warm-orphan state directly, the same technique as
// TestWarmOrphanChildLineageKeepsDurableParentID: spawn a child under a
// root in one harness, then adopt ONLY the child's reload into a second,
// independent harness's fresh SessionManager via ReportTurnStart — root
// stays untracked there, so adoptReloadedLocked takes its
// durable-TaskDepth branch (attachTo never set, info.ParentID empty).
func TestGenericTurnRoutesRejectWarmOrphanChild(t *testing.T) {
	dir := t.TempDir()
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("child done")}}
	h1 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)

	resp, data := h1.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h1.do("POST", "/session", map[string]string{
		"parent_id": root.ID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "child/m1",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)
	waitForLineageStatus(t, h1, child.ID, "done", 2*time.Second)

	h2 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)
	reloaded, err := engine.LoadSession(engine.Config{SessionDir: dir, Model: message.ModelRef{Provider: "child", Model: "m1"}}, child.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	h2.srv.sessMgr.ReportTurnStart(reloaded)
	if info, ok := h2.srv.sessMgr.Info(child.ID); !ok || info.ParentID != "" {
		t.Fatalf("child info = %+v (ok=%v), want ParentID empty (warm orphan) — test setup invalid", info, ok)
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"prompt_async", "POST", "/session/" + child.ID + "/prompt_async", map[string]any{"parts": []map[string]string{{"type": "text", "text": "hi"}}}},
		{"goal", "POST", "/session/" + child.ID + "/goal", map[string]string{"condition": "done"}},
		{"enqueue", "POST", "/session/" + child.ID + "/enqueue", map[string]any{"parts": []map[string]string{{"type": "text", "text": "hi"}}, "seq": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := h2.do(tc.method, tc.path, tc.body)
			if resp.StatusCode != 409 {
				t.Errorf("%s %s status = %d, want 409 (warm orphan must be recognized as a managed child): %s", tc.method, tc.path, resp.StatusCode, data)
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
	// The queued prompt must run too, not be lost. until=idle spans the
	// in-flight resume turn AND the queue drain behind it (waitSnapshot
	// folds in queueDrainPending), so idle here means the session has
	// stopped cycling — the one moment at which "the queued prompt never
	// ran" is a real verdict rather than a guess about elapsed time.
	h.waitIdle(root.ID)
	resp, data = h.do("GET", "/session/"+root.ID+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "concurrent prompt") {
		t.Fatalf("queued prompt never appears to have been delivered: %s", data)
	}
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
	// The resume-trigger message must carry "origin":"engine" on the wire —
	// this is the production path (resumeSessionForTaskNotification's
	// ExternalRunner, via runOrQueueText's idle-no-queue branch) boxes'
	// console actually observes, not just the engine-package unit test's
	// no-ExternalRunner fallback. See message.Message.Origin's own doc
	// comment: it is what lets the console render this as a system notice
	// instead of a human-typed bubble.
	if !strings.Contains(string(data), `"origin":"engine"`) {
		t.Errorf("resume-trigger message missing origin:engine on the wire: %s", data)
	}
}
