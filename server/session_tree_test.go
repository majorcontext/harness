package server

import (
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
