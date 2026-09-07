package server

import "testing"

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
