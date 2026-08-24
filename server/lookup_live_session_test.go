package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// newGatedMultiProviderHarness is newGatedHarness's (queue_toolcall_boundary_test.go)
// multi-provider sibling: it wires several scripted providers into one
// registry — root and child use DIFFERENT providers/models, same as
// multiProviderHarness (session_tree_test.go) — while every session (root
// or child alike) still gets the same test-only "gate" tool, a real
// blocking tool call rather than a blocking provider Stream. A child spawned
// from a root built this way inherits Tools (and everything else) through
// Session.configSnapshot's full Config copy, the same inheritance
// SessionManager.Spawn's childCfg derivation (session_manager.go) already
// relies on in production.
func newGatedMultiProviderHarness(t *testing.T, entered, release chan struct{}, providers ...provider.Provider) *harness {
	t.Helper()
	dir := t.TempDir()
	const token = "secret-run-token"
	reg := provider.Registry{}
	for _, p := range providers {
		reg[p.Name()] = p
	}
	gate := engine.Tool{
		Def: provider.ToolDef{
			Name:        "gate",
			Description: "test-only tool that blocks until released",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Run: func(ctx context.Context, s *engine.Session, args json.RawMessage) (message.Parts, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return message.Parts{&message.Text{Text: "gate done"}}, nil
		},
	}
	var srv *Server
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:  reg,
			Model:      m,
			SessionDir: dir,
			OnEvent:    func(ev engine.Event) { srv.Publish(ev) },
			Tools:      []engine.Tool{gate},
		}
	}
	opts := Options{
		SessionDir:        dir,
		RunToken:          token,
		Version:           "9.9.9",
		HeartbeatInterval: 20 * time.Millisecond,
		NewSession: func(m message.ModelRef, workDir string, parentSession string) (*engine.Session, error) {
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parentSession
			return engine.NewSession(cfg), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(mkCfg(message.ModelRef{}), id)
		},
	}
	var err error
	srv, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

// TestLiveChildToolCallNotSynthesizedAsOrphanError is the regression test
// for Server.lookup's ordering bug: a subagent (task-spawned child) mid a
// long-running tool call — the box console's own repro was a `bash sleep
// 45` — used to render as CRASHED/FAILED for as long as it kept running.
//
// A child is never registered in s.sessions (root-only bookkeeping), so
// every GET against one used to fall straight to a COLD s.opts.LoadSession
// reread — before ever consulting sessMgr, this process's own live,
// in-memory tracker. That cold reread's scanLog replay runs
// message.ResolveOrphanToolCalls (store.go) unconditionally: a crash-repair
// backstop that synthesizes an is_error tool_result for ANY tool_use with no
// matching result yet. Correct for a session that genuinely died mid-turn;
// wrong for one simply still running its own in-flight tool call — which is
// exactly what a child executing a long tool call looks like on disk at any
// instant before that call returns (the assistant's tool_call message
// persists before the tool ever runs, same as production — see
// Session.Prompt). The fix (Server.lookup) checks sessMgr FIRST, so a
// GET while the tool is still genuinely executing sees the live, unrepaired
// in-flight shape: the tool_call present, no tool-role result at all yet —
// never a fabricated error.
func TestLiveChildToolCallNotSynthesizedAsOrphanError(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	rootProv := &scriptedProvider{name: "root"}
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{
		toolCallTurn("tc1", "gate", `{}`),
		asstTurn("done sleeping"),
	}}
	h := newGatedMultiProviderHarness(t, entered, release, rootProv, childProv)

	rootID := h.createSession("root/m1")

	childID, err := h.srv.SessionManager().Spawn(engine.SpawnOptions{
		ParentID: rootID,
		Prompt:   "run gate",
		Model:    message.ModelRef{Provider: "child", Model: "m1"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("gate tool never entered — child turn never started")
	}

	// The gate tool is now genuinely, currently executing: the child's own
	// durable log has an assistant message carrying tc1's tool_call and NO
	// matching tool-role result yet — a real on-disk dangling tool_use, the
	// exact shape the old LoadSession-first lookup order would (mis)repair.
	resp, data := h.do("GET", "/session/"+childID+"/message", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET child message status %d: %s", resp.StatusCode, data)
	}
	body := string(data)
	if !strings.Contains(body, `"call_id":"tc1"`) {
		t.Fatalf("child transcript missing the in-flight tool call entirely: %s", body)
	}
	if strings.Contains(body, "synthetic-orphan") {
		t.Errorf("live in-flight child's tool call was synthesized as a crashed/orphaned one: %s", body)
	}
	if strings.Contains(body, `"is_error":true`) {
		t.Errorf("live in-flight child's tool call rendered with a fabricated error result: %s", body)
	}
	if strings.Contains(body, `"role":"tool"`) {
		t.Errorf("live in-flight child's tool call already has a result — test setup did not actually catch it mid-flight: %s", body)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, data := h.do("GET", "/session/"+childID+"/message", nil)
		if strings.Contains(string(data), "gate done") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("gate tool call never completed after release")
}
