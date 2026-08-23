package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func TestTaskToolRegisteredOnRootUnderDepthLimit(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	if _, ok := root.tools[taskToolName]; !ok {
		t.Errorf("task tool not registered on root: %v", toolNames(root))
	}
}

func TestTaskToolAbsentWithoutSessionManager(t *testing.T) {
	s := NewSession(Config{WorkDir: t.TempDir()})
	if _, ok := s.tools[taskToolName]; ok {
		t.Errorf("task tool registered without a SessionManager")
	}
}

func TestTaskToolWithheldAtDepthLimit(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 1, 0) // depth limit 1
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	child, _ := mgr.Session(childID)
	if _, ok := child.tools[taskToolName]; ok {
		t.Errorf("task tool present on child at depth limit: %v", toolNames(child))
	}
}

func TestTaskToolLeafDefinitionExcludesTaskEvenBelowLimit(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0) // plenty of depth headroom
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{
		ParentID:  root.ID,
		Prompt:    "look around",
		Model:     modelFor("child"),
		ToolNames: readOnlyTools, // explore/plan preset: no "task" in the list
		AgentType: AgentExplore,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	child, _ := mgr.Session(childID)
	if _, ok := child.tools[taskToolName]; ok {
		t.Errorf("task tool present on a leaf (explore) definition: %v", toolNames(child))
	}
}

func TestRunTaskToolSpawnsChildAndReturnsImmediately(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns(AgentExplore, doneTurn("found it in main.go")),
	))

	raw, _ := json.Marshal(map[string]string{
		"agent":  AgentExplore,
		"prompt": "find the entry point",
		"model":  modelFor(AgentExplore).String(),
	})
	parts, err := runTaskTool(root, raw)
	if err != nil {
		t.Fatalf("runTaskTool: %v", err)
	}
	var result taskToolResult
	if err := json.Unmarshal([]byte(parts.Text()), &result); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, parts.Text())
	}
	if result.SessionID == "" {
		t.Fatal("result.SessionID empty")
	}
	if result.Agent != AgentExplore {
		t.Errorf("result.Agent = %q, want %q", result.Agent, AgentExplore)
	}

	info, ok := mgr.Info(result.SessionID)
	if !ok {
		t.Fatalf("Info(%s) not found", result.SessionID)
	}
	if info.ParentID != root.ID {
		t.Errorf("parent = %q, want %q", info.ParentID, root.ID)
	}
	if info.AgentType != AgentExplore {
		t.Errorf("AgentType = %q, want %q", info.AgentType, AgentExplore)
	}
}

func TestRunTaskToolUnknownAgentIsError(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))

	raw, _ := json.Marshal(map[string]string{"agent": "not-a-real-agent", "prompt": "go"})
	if _, err := runTaskTool(root, raw); err == nil {
		t.Error("runTaskTool with unknown agent: want error, got nil")
	} else if !strings.Contains(err.Error(), "not-a-real-agent") {
		t.Errorf("error = %v, want it to name the unknown agent", err)
	}
}

func TestRunTaskToolMissingSessionManagerIsError(t *testing.T) {
	s := NewSession(Config{
		Providers: provider.Registry{"test": scriptedTurns("test", nil)},
		Model:     modelFor("test"),
		WorkDir:   t.TempDir(),
	})
	raw, _ := json.Marshal(map[string]string{"agent": AgentExplore, "prompt": "go"})
	if _, err := runTaskTool(s, raw); err == nil {
		t.Error("runTaskTool without a SessionManager: want error, got nil")
	}
}

func TestRunTaskToolDepthLimitSurfacesAsCleanError(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 1, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	child, _ := mgr.Session(childID)

	raw, _ := json.Marshal(map[string]string{"agent": AgentGeneralPurpose, "prompt": "go deeper"})
	_, err = runTaskTool(child, raw)
	if err == nil {
		t.Fatal("runTaskTool at depth limit: want error, got nil")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("error = %v, want it to mention the depth limit", err)
	}
}

// TestTaskDeliveryParentIdleTriggersResumeTurn is the delivery integration
// test the design doc's testing strategy calls for: a child completing
// while its parent is idle causes the engine to initiate a resume turn on
// the parent, carrying the notification as an EngineContext part.
func TestTaskDeliveryParentIdleTriggersResumeTurn(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	rootProv := &scriptedProvider{name: "root", turns: [][]provider.Event{
		// This is the ENGINE-INITIATED resume turn — no explicit Send call
		// in this test ever asks the root to run again.
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ack: child finished"}),
	}}
	root := mgr.NewRoot(managedConfig("root", rootProv, scriptedTurns("child", doneTurn("the answer is 42"))))

	// Establish real history so withAmbientStatus has a user message to
	// attach the EngineContext part to, and so the root can go properly
	// idle afterward.
	if _, err := mgr.Send(context.Background(), root.ID, "start"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForStatus(t, mgr, root.ID, StatusIdle, time.Second)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go find it", Model: modelFor("child"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// The child's completion must have driven the root back to running
	// and then idle again, ALL WITHOUT any Send call from this test.
	waitForStatus(t, mgr, root.ID, StatusIdle, time.Second)

	if len(rootProv.requests) != 2 {
		t.Fatalf("root received %d requests, want 2 (initial Send + engine-initiated resume)", len(rootProv.requests))
	}
	resumeReq := rootProv.requests[1]
	var found *message.EngineContext
	for i := len(resumeReq.Messages) - 1; i >= 0; i-- {
		msg := resumeReq.Messages[i]
		if msg.Role != message.RoleUser {
			continue
		}
		for _, p := range msg.Parts {
			if ec, ok := p.(*message.EngineContext); ok {
				found = ec
			}
		}
		break
	}
	if found == nil {
		t.Fatalf("resume request carries no EngineContext part on its newest user message: %+v", resumeReq.Messages)
	}
	if !strings.Contains(found.Text, "the answer is 42") {
		t.Errorf("EngineContext text = %q, want it to include the child's result", found.Text)
	}
	if !strings.Contains(found.Text, childID) {
		t.Errorf("EngineContext text = %q, want it to include the child id %q", found.Text, childID)
	}

	// The resume turn's own newest user message must be the synthetic
	// trigger, a REAL history entry — never silently invented text the
	// transcript can't account for.
	var lastUserText string
	for i := len(resumeReq.Messages) - 1; i >= 0; i-- {
		if resumeReq.Messages[i].Role == message.RoleUser {
			lastUserText = resumeReq.Messages[i].Parts.Text()
			break
		}
	}
	if lastUserText != taskResumeTriggerText {
		t.Errorf("resume trigger text = %q, want %q", lastUserText, taskResumeTriggerText)
	}

	// The final assistant reply came from the SECOND scripted turn.
	rootInfo, _ := mgr.Info(root.ID)
	if rootInfo.Status != StatusIdle {
		t.Errorf("root status = %s, want idle", rootInfo.Status)
	}
}

// TestTaskDeliveryQueuedNotificationsDrainExactlyOnce proves the OTHER half
// of queue-or-resume directly against the primitive streamTurn calls on
// every model request (drainTaskNotificationsSegment): a session mid-turn
// never gets an extra resume turn injected — its NEXT streamTurn call
// (whether that is later in the same tool loop or the next external
// Prompt/Send) is what picks the notification up, and picks it up exactly
// once, several notifications combining into one segment.
func TestTaskDeliveryQueuedNotificationsDrainExactlyOnce(t *testing.T) {
	s := NewSession(Config{WorkDir: t.TempDir()})
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_x", Status: StatusDone, Result: "hi"})
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_y", Status: StatusFailed, FailReason: "boom"})

	seg := s.drainTaskNotificationsSegment()
	if !strings.Contains(seg, "ses_x") || !strings.Contains(seg, "hi") {
		t.Errorf("segment missing first notification: %q", seg)
	}
	if !strings.Contains(seg, "ses_y") || !strings.Contains(seg, "boom") {
		t.Errorf("segment missing second notification: %q", seg)
	}
	if again := s.drainTaskNotificationsSegment(); again != "" {
		t.Errorf("second drain = %q, want empty (exactly-once delivery)", again)
	}
}
