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

	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "spawned" {
		t.Errorf("events = %v, want [spawned]", got)
	}
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
// both about Children specifically:
//
//   - It used to set Children to []string{} — indistinguishable on the
//     wire from a WARM, genuinely childless node's real empty list. For a
//     MID-TREE parent (this test's child, which itself spawns a
//     grandchild below) that is exactly wrong: the child has a real,
//     live grandchild on disk, but a caller reading "children":[] from a
//     cold GET would reasonably conclude it has none — an affirmatively
//     wrong answer, worse than an honestly unknown one.
//   - The fix for THAT (giving Children omitempty, so the cold branch
//     could leave it nil and have it vanish from the wire) went one step
//     too far: omitempty collapses nil and a genuinely empty non-nil
//     slice to the same "absent" wire shape, so a WARM, truly childless
//     node ALSO started omitting the field — indistinguishable from this
//     cold branch's own "unknown." Children now has NO omitempty
//     instead: the cold branch's nil serializes as an explicit
//     "children":null (present, but honestly unknown — distinct from
//     both "known: zero" and "known: non-zero"), while the warm branch
//     (lineageJSONFor) normalizes nil to []string{} so a real empty list
//     still reads "children":[].
//
// Proves "children" is present as JSON null on the cold path — neither
// omitted nor an affirmative empty list — even though a real child (the
// grandchild) exists.
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
	// Fields with no durable source must be OMITTED, not guessed.
	if _, ok := cold.Lineage["status"]; ok {
		t.Errorf("cold lineage.status = %v, want omitted (no durable source)", cold.Lineage["status"])
	}
	if _, ok := cold.Lineage["depth"]; ok {
		t.Errorf("cold lineage.depth = %v, want omitted (no durable source)", cold.Lineage["depth"])
	}
	// children has no omitempty (see its own doc comment, handlers.go) —
	// present but explicitly null on the cold path, not omitted and not
	// an affirmative []. The child genuinely has a live grandchild on
	// disk, so an affirmative empty list would be actively wrong, not
	// just unknown — and simply omitting the key again would reopen the
	// exact warm/cold ambiguity the field's own fix exists to close.
	v, ok := cold.Lineage["children"]
	if !ok {
		t.Error("cold lineage.children key missing entirely, want present as JSON null")
	}
	if v != nil {
		t.Errorf("cold lineage.children = %v, want null (unknown) — the child genuinely has a live grandchild on disk, so an affirmative list would be actively wrong", v)
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

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, data := h.do("GET", "/session/"+root.ID+"/message", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
		}
		if len(data) > 2 && string(data) != "[]" && strings.Contains(string(data), "hello back") {
			// A session.send delivery is a genuine operator-authored
			// message, never the engine's own synthetic resume trigger —
			// it must not carry origin:engine (see sendTextToRoot's own
			// doc comment and message.Message.Origin's).
			if strings.Contains(string(data), `"origin":"engine"`) {
				t.Errorf("session.send message wrongly carries origin:engine: %s", data)
			}
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

	// grand must be untouched collateral — still running, not failed or
	// canceled.
	time.Sleep(20 * time.Millisecond) // let any wrongful cascade land, if the fix regressed
	resp, data = h.do("GET", "/session/"+grand.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get grand status %d: %s", resp.StatusCode, data)
	}
	var grandInfo struct {
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &grandInfo)
	if grandInfo.Lineage["status"] != "running" {
		t.Errorf("grand lineage.status = %v, want %q (aborting the already-done mid must not touch its running grandchild)", grandInfo.Lineage["status"], "running")
	}

	// mid must still be sendable — its context was never canceled.
	resp, data = h.do("POST", "/session/"+mid.ID+"/send", map[string]string{"text": "follow-up"})
	if resp.StatusCode != 202 {
		t.Fatalf("send to mid after its own no-op abort status %d, want 202: %s", resp.StatusCode, data)
	}
	waitForLineageStatus(t, h, mid.ID, "done", 2*time.Second)
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

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, data := h.do("GET", "/session/"+rootB.ID+"/message", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("get B messages status %d: %s", resp.StatusCode, data)
		}
		if strings.Contains(string(data), resumeTriggerText) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("B never ran a resume turn once the workdir freed — queue-or-resume stayed dead after the first refusal: %s", data)
		}
		time.Sleep(2 * time.Millisecond)
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
