package server

import (
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// A consumer reading only events.jsonl must be able to place a child
// session under its parent. Without this record it sees turn.end for a
// session id it has never heard of.
func TestChildSpawnIsJournaled(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	h.srv.onChildSpawn("ses_parent", "ses_child", "explore")

	var found *Event
	h.srv.mu.Lock()
	for i := range h.srv.journal {
		if h.srv.journal[i].Type == evtSessionSpawned {
			found = &h.srv.journal[i]
			break
		}
	}
	h.srv.mu.Unlock()

	if found == nil {
		t.Fatal("no session.spawned record was journaled")
	}
	if found.SessionID != "ses_child" {
		t.Errorf("SessionID = %q, want the child id", found.SessionID)
	}
	if found.ParentSessionID != "ses_parent" {
		t.Errorf("ParentSessionID = %q, want the parent id", found.ParentSessionID)
	}
	if found.AgentType != "explore" {
		t.Errorf("AgentType = %q, want %q", found.AgentType, "explore")
	}
	if found.Seq == 0 {
		t.Error("Seq = 0, want a durable sequence number")
	}
}

// A spawn through the real server must journal the record, not merely be
// capable of journaling it. TestChildSpawnIsJournaled calls onChildSpawn
// directly, so it passes even when nothing installs the observer — deleting
// SetChildSpawnObserver in server.New leaves the whole suite green. This
// test is what fails in that case, and it is the wiring the task exists for:
// the `task` tool and this HTTP route both reach the record through
// SessionManager.Spawn.
func TestSpawnThroughServerJournalsTheRecord(t *testing.T) {
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("done")}}
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
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)

	// Wait for the child's own turn to settle before the test body ends.
	// A child's Prompt runs on SessionManager's node.ctx, independent of the
	// test server's connection tracking, so an unsettled child can still be
	// writing its session log when t.TempDir's cleanup removes the directory
	// — see TestSpawnResponseReportsBusyNotIdle's comment on the same flake.
	waitForLineageStatus(t, h, child.ID, "done", 2*time.Second)

	var found *Event
	h.srv.mu.Lock()
	for i := range h.srv.journal {
		if h.srv.journal[i].Type == evtSessionSpawned && h.srv.journal[i].SessionID == child.ID {
			found = &h.srv.journal[i]
			break
		}
	}
	h.srv.mu.Unlock()

	if found == nil {
		t.Fatal("spawning a child through the server journaled no session.spawned record")
	}
	if found.ParentSessionID != root.ID {
		t.Errorf("ParentSessionID = %q, want the root id %q", found.ParentSessionID, root.ID)
	}
	if found.AgentType != engine.AgentExplore {
		t.Errorf("AgentType = %q, want %q", found.AgentType, engine.AgentExplore)
	}

	// session.spawned must be the FIRST durable record for a child id. A
	// consumer reading events.jsonl in seq order that meets session.status
	// for a session it cannot yet place sees exactly the unplaceable id this
	// record exists to prevent — and the consuming design answers an
	// apparent gap with a full re-bootstrap, so getting this backwards costs
	// a re-bootstrap per subagent spawn.
	var firstStatusSeq int64
	h.srv.mu.Lock()
	for i := range h.srv.journal {
		ev := &h.srv.journal[i]
		if ev.SessionID == child.ID && ev.Type == evtSessionStatus {
			firstStatusSeq = ev.Seq
			break
		}
	}
	h.srv.mu.Unlock()

	if firstStatusSeq == 0 {
		t.Fatal("child journaled no session.status record to order against")
	}
	if found.Seq >= firstStatusSeq {
		t.Errorf("session.spawned seq = %d, first session.status seq = %d; spawned must come first",
			found.Seq, firstStatusSeq)
	}
}
