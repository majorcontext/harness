package engine

import (
	"context"
	"testing"
)

// The task tool spawns through SessionManager.Spawn, never through the
// server's HTTP handler, so an observer installed on the server alone
// misses every subagent an agent spawns itself.
func TestSpawnNotifiesChildSpawnObserver(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("child result")),
	))

	type spawnCall struct{ parent, child, agent string }
	seen := make(chan spawnCall, 1)
	mgr.SetChildSpawnObserver(func(parentID, childID, agentType string) {
		seen <- spawnCall{parentID, childID, agentType}
	})

	childID, err := mgr.Spawn(SpawnOptions{
		ParentID:  root.ID,
		Prompt:    "go",
		AgentType: "explore",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	got := <-seen
	if got.parent != root.ID || got.child != childID || got.agent != "explore" {
		t.Fatalf("observer got %+v, want parent %q child %q agent %q",
			got, root.ID, childID, "explore")
	}
}

func TestSpawnDoesNotNotifyObserverOnRefusal(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)

	mgr.SetChildSpawnObserver(func(string, string, string) {
		t.Error("observer fired for a spawn that created no session")
	})

	if _, err := mgr.Spawn(SpawnOptions{ParentID: "ses_unknown", Prompt: "go"}); err == nil {
		t.Fatal("Spawn with an unknown parent succeeded, want ErrUnknownSession")
	}
}
