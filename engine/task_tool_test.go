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

// TestGrandchildRegistryIsIntersectionNeverWiderThanParent is the
// regression test for an architecture-review BLOCKER: Spawn derived the
// child's registry from the SESSION-DEFAULT full set (filtered only by
// the definition's own opts.ToolNames), never from the PARENT's actual
// effective registry — a privilege-escalation edge. A custom
// definition like `tools: read_file, task` (read-only plus the ability
// to spawn) spawning a general-purpose child (opts.ToolNames == nil for
// that built-in, meaning "no additional restriction") used to hand that
// child the FULL default set, bash included — even though the
// RESTRICTED parent spawning it could never reach bash itself. The
// spec's own table says general-purpose gets "the parent's full tool
// set," not "the session's." Proves the grandchild's registry is
// exactly the intersection: read_file (and task, since depth allows
// it) — nothing more, in particular never bash/write_file/edit_file.
func TestGrandchildRegistryIsIntersectionNeverWiderThanParent(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0) // plenty of depth headroom
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("mid", doneTurn("mid done")),
		scriptedTurns("grand", doneTurn("grand done")),
	))

	// A custom, non-leaf, read-only-plus-spawn definition — the exact
	// shape the review named: read_file and task only.
	midID, err := mgr.Spawn(SpawnOptions{
		ParentID: root.ID, Prompt: "go", Model: modelFor("mid"),
		AgentType: "custom-read-and-spawn",
		ToolNames: []string{"read_file", taskToolName},
	})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	waitForStatus(t, mgr, midID, StatusDone, time.Second)
	mid, _ := mgr.Session(midID)
	if _, ok := mid.tools["bash"]; ok {
		t.Fatalf("test setup: mid unexpectedly has bash: %v", toolNames(mid))
	}

	// A general-purpose grandchild from mid — opts.ToolNames == nil,
	// exactly like the built-in AgentGeneralPurpose definition
	// (Tools: nil, "no ADDITIONAL restriction").
	grandID, err := mgr.Spawn(SpawnOptions{
		ParentID: midID, Prompt: "go deeper", Model: modelFor("grand"),
		AgentType: AgentGeneralPurpose,
	})
	if err != nil {
		t.Fatalf("Spawn grand: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)
	grand, _ := mgr.Session(grandID)

	if _, ok := grand.tools["bash"]; ok {
		t.Errorf("grandchild regained bash despite its restricted parent never having it: %v", toolNames(grand))
	}
	if _, ok := grand.tools["write_file"]; ok {
		t.Errorf("grandchild regained write_file: %v", toolNames(grand))
	}
	if _, ok := grand.tools["edit_file"]; ok {
		t.Errorf("grandchild regained edit_file: %v", toolNames(grand))
	}
	if _, ok := grand.tools["read_file"]; !ok {
		t.Errorf("grandchild missing read_file, want it inherited from mid: %v", toolNames(grand))
	}
	// Every tool the grandchild has must ALSO be one mid effectively
	// had — the intersection property, checked directly rather than by
	// enumerating individual names.
	for name := range grand.tools {
		if _, ok := mid.tools[name]; !ok {
			t.Errorf("grandchild has %q, which its parent mid did not have — registry is wider than the parent's: %v", name, toolNames(grand))
		}
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

// TestRunTaskToolUnconfiguredModelOverrideIsSynchronousError is the
// regression test for a live review finding: a `task` call's model
// override was parsed (ParseModelRef, well-formedness only) but never
// checked against the configured providers, unlike the `model` session
// tool's own identical override (runModelTool, ModelSupported). An
// override naming a provider nothing registers used to sail straight
// through Spawn — burning a concurrency slot and a session log — and only
// fail later, at the child's own first turn, surfacing to the caller as a
// delayed "[tasks: ... failed: ...]" notification instead of an
// immediate, synchronous tool error — proven here by asserting the error
// return itself (pre-fix, this call returned nil: Spawn has no provider
// check of its own, so it always succeeded and only failed much later,
// asynchronously, inside the child's own first turn).
func TestRunTaskToolUnconfiguredModelOverrideIsSynchronousError(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns(AgentExplore, doneTurn("found it")),
	))

	raw, _ := json.Marshal(map[string]string{
		"agent":  AgentExplore,
		"prompt": "find the entry point",
		"model":  "totally-unconfigured-provider/some-model",
	})
	if _, err := runTaskTool(root, raw); err == nil {
		t.Error("runTaskTool with an unconfigured provider override: want error, got nil")
	} else if !strings.Contains(err.Error(), "totally-unconfigured-provider") {
		t.Errorf("error = %v, want it to name the unconfigured provider", err)
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

// Low-level checkout/commit/requeue mechanics are covered in
// taskdelivery_test.go.
