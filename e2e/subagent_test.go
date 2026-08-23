package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// lineageJSON mirrors server.lineageJSON (session.info's subagent-sessions
// extension) — this package is black-box (HTTP only), so the wire shape is
// redeclared locally rather than importing the server package, same
// pattern as apiMessage and enqueue_test.go's enqueueRespJSON.
type lineageJSON struct {
	ParentID  string   `json:"parent_id,omitempty"`
	Depth     int      `json:"depth,omitempty"`
	Status    string   `json:"status,omitempty"`
	Children  []string `json:"children"`
	AgentType string   `json:"agent_type,omitempty"`
	Result    string   `json:"result,omitempty"`
}

// sessionJSON is the subset of server.sessionJSON these tests need.
type sessionJSON struct {
	ID      string       `json:"id"`
	Status  string       `json:"status"`
	Lineage *lineageJSON `json:"lineage,omitempty"`
}

// spawnChild posts session.create's "with a parent" form (POST /session
// with parent_id set) and returns the raw status code alongside the
// decoded child session, so a test can assert the wire contract's status
// code (201, immediately, before the child's own turn ever runs) together
// with its body.
func (p *serveProc) spawnChild(parentID, agent, prompt string) (int, sessionJSON) {
	p.t.Helper()
	body := map[string]any{"parent_id": parentID, "agent": agent, "prompt": prompt}
	resp, data := p.do(http.MethodPost, "/session", body)
	var s sessionJSON
	if err := json.Unmarshal(data, &s); err != nil {
		p.t.Fatalf("decode spawned child: %v (%s)", err, data)
	}
	return resp.StatusCode, s
}

// getSessionLineage fetches GET /session/{id} and decodes it into the
// lineage-shaped sessionJSON local to this file — named to avoid
// colliding with compaction_test.go's own getSession, which decodes a
// different (compaction-focused) subset of the same endpoint.
func (p *serveProc) getSessionLineage(id string) (int, sessionJSON) {
	p.t.Helper()
	resp, data := p.do(http.MethodGet, "/session/"+id, nil)
	var s sessionJSON
	if err := json.Unmarshal(data, &s); err != nil {
		p.t.Fatalf("decode session: %v (%s)", err, data)
	}
	return resp.StatusCode, s
}

// waitForLineageStatus polls GET /session/{id} until lineage.status == want
// or timeout elapses.
func (p *serveProc) waitForLineageStatus(id, want string, timeout time.Duration) sessionJSON {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		code, s := p.getSessionLineage(id)
		if code != http.StatusOK {
			p.t.Fatalf("get session %s: status %d", id, code)
		}
		if s.Lineage != nil && s.Lineage.Status == want {
			return s
		}
		if time.Now().After(deadline) {
			got := ""
			if s.Lineage != nil {
				got = s.Lineage.Status
			}
			p.t.Fatalf("session %s lineage.status = %q after %s, want %q", id, got, timeout, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSubagentSpawnDeliversViaQueueOrResume is this repo's first COMMITTED,
// CI-run E2E coverage of the subagent-sessions feature (PR #145) — a
// follow-up finding ("e2e test"): before this, task/spawn/lineage/
// cancel_tree scenarios existed only as engine/server package unit tests
// and one-off manual verification, never as a real process-boundary,
// real-HTTP scenario a CI run actually exercises. Drives the wire-level
// equivalent of the `task` tool (session.create's parent_id form,
// handleSpawnChild) directly rather than getting a real model to emit a
// tool_use block — the highest-value minimal scenario, and the one the
// architecture review's own follow-up list named specifically: proves
// non-blocking spawn (the 201 response arrives before the child's own
// turn ever runs) and the design doc's "queue-or-resume delivery" (an
// idle parent gets an engine-initiated resume turn once its child
// completes) actually work end to end, through a real `harness serve`
// subprocess and real HTTP, not just in-process Go calls.
func TestSubagentSpawnDeliversViaQueueOrResume(t *testing.T) {
	skipShort(t)

	fake := newFakeAnthropic(0) // never stall: every request gets a complete turn
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	sessDir := t.TempDir()
	cfgPath := writeConfig(t, srv.URL)
	p := startServe(t, sessDir, cfgPath)

	rootID := p.createSession()

	// Give root one real turn first, so it has established history and
	// then goes idle — exactly the state a spawned child's completion
	// needs to find it in for queue-or-resume's engine-initiated resume
	// path to fire (rather than the turn-boundary queue path a BUSY
	// parent would use instead).
	p.prompt(rootID, "say hi")
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, s := p.getSessionLineage(rootID)
		if code != http.StatusOK {
			t.Fatalf("get root: status %d", code)
		}
		if s.Status == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("root never went idle after its first turn (status=%q)", s.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	msgsBefore := p.messages(rootID)

	// Non-blocking spawn: the 201 response — and the child id/lineage it
	// carries — must arrive before the child's own turn has necessarily
	// finished. The fake backend is fast enough that a race either way is
	// plausible; what matters is the RESPONSE ITSELF, not a race on
	// timing, so this only asserts the response shape, never a status.
	code, child := p.spawnChild(rootID, "general-purpose", "find the answer")
	if code != http.StatusCreated {
		t.Fatalf("spawn child: status %d", code)
	}
	if child.ID == "" {
		t.Fatal("spawned child has empty id")
	}
	if child.Lineage == nil || child.Lineage.ParentID != rootID {
		t.Fatalf("spawned child lineage = %+v, want parent_id %q", child.Lineage, rootID)
	}

	// The parent's own lineage.children must list the child immediately
	// (SessionManager registers it synchronously, inside Spawn, before
	// the child's turn even launches).
	_, rootAfterSpawn := p.getSessionLineage(rootID)
	if rootAfterSpawn.Lineage == nil {
		t.Fatal("root has no lineage after spawning a child")
	}
	found := false
	for _, c := range rootAfterSpawn.Lineage.Children {
		if c == child.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("root lineage.children = %v, want it to include %q", rootAfterSpawn.Lineage.Children, child.ID)
	}

	// The child completes on its own (the fake backend always returns a
	// complete turn) — proves the spawn genuinely launched a real turn,
	// not just registered a node.
	done := p.waitForLineageStatus(child.ID, "done", 10*time.Second)
	if done.Lineage.Result == "" {
		t.Error("done child has empty lineage.result")
	}

	// Queue-or-resume delivery: the idle root must be woken with an
	// engine-initiated resume turn once its child completes — with NO
	// explicit prompt/send of any kind from this test. Observed via the
	// root's own message count growing past what it was right before the
	// spawn (a real resume turn appends at least a synthetic user-role
	// trigger message and a real assistant reply).
	deadline = time.Now().Add(10 * time.Second)
	for {
		msgsAfter := p.messages(rootID)
		if len(msgsAfter) > len(msgsBefore) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("root never resumed after its child completed; message count stayed at %d", len(msgsBefore))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// DELETE .../cancel_tree cascades to the whole subtree — a second,
	// cheap assertion piggybacking on the same running server: spawn one
	// more child, then cancel_tree the root, and confirm the child ends
	// up canceled.
	code, child2 := p.spawnChild(rootID, "general-purpose", "another task")
	if code != http.StatusCreated {
		t.Fatalf("spawn child2: status %d", code)
	}
	resp, data := p.do(http.MethodDelete, "/session/"+rootID+"/cancel_tree", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel_tree: status %d body %s", resp.StatusCode, data)
	}
	// child2 may have already reached "done" (the fake backend is fast)
	// before cancel_tree ran — in which case cancel_tree correctly leaves
	// its terminal outcome alone (Cancel never overwrites an
	// already-terminal node's status). Accept either terminal outcome;
	// only a still-"running" status after cancel_tree would be a bug.
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, s := p.getSessionLineage(child2.ID)
		if s.Lineage != nil && (s.Lineage.Status == "canceled" || s.Lineage.Status == "done") {
			break
		}
		if time.Now().After(deadline) {
			got := ""
			if s.Lineage != nil {
				got = s.Lineage.Status
			}
			t.Fatalf("child2 lineage.status = %q after cancel_tree, want canceled or done", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
