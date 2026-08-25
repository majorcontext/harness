// Tests for the provider-exhausted child outcome: an ACCOUNT-level supply
// wall (usage limit, quota, credit balance) is not the child's failure. It
// is fleet-wide and temporal, so the parent must preserve the child and
// resume it later instead of respawning a sibling into the same wall.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// exhaustionError builds the live incident's own error, as the anthropic
// adapter classifies it: a permanent-marked provider.Error of kind
// ErrKindProviderExhausted carrying the recover-at hint.
func exhaustionError(hint string) error {
	return provider.MarkPermanent(&provider.Error{
		Kind:        provider.ErrKindProviderExhausted,
		Raw:         "anthropic: You have reached your specified API usage limits. You will regain access on " + hint + ". (invalid_request_error, HTTP 400)",
		RecoverHint: hint,
	})
}

// TestProviderExhaustedChildIsClassified proves the node and its snapshot
// carry the structured kind, so every reader — task status, the wire's
// lineage, the journal — can branch on it without parsing prose.
func TestProviderExhaustedChildIsClassified(t *testing.T) {
	mgr, root, childID := spawnFailedChild(t, exhaustionError("2026-09-01"))

	node, _, err := mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo: %v", err)
	}
	if node.Status != StatusFailed {
		t.Errorf("Status = %s, want %s (the status vocabulary is unchanged)", node.Status, StatusFailed)
	}
	if node.FailKind != FailKindProviderExhausted {
		t.Errorf("FailKind = %q, want %q", node.FailKind, FailKindProviderExhausted)
	}
	if !strings.Contains(node.FailReason, "2026-09-01") {
		t.Errorf("FailReason = %q, want it to carry the recover-at hint", node.FailReason)
	}
	if !strings.Contains(node.FailReason, "usage limits") {
		t.Errorf("FailReason = %q, want it to carry the provider cause", node.FailReason)
	}
}

// TestOrdinaryChildFailureHasNoFailKind is the surplus-direction guard: a
// plain failure must not claim an exhaustion, or a parent would sit and
// wait for a wall that does not exist.
func TestOrdinaryChildFailureHasNoFailKind(t *testing.T) {
	mgr, root, childID := spawnFailedChild(t, provider.MarkPermanent(errors.New("anthropic: messages.0: unexpected field")))

	node, _, err := mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo: %v", err)
	}
	if node.FailKind != "" {
		t.Errorf("FailKind = %q, want empty for an ordinary permanent failure", node.FailKind)
	}
}

// TestRateLimitExhaustionIsProviderExhausted covers the second entry point
// to the same wall: a rate limit that survived the whole retry budget. The
// engine reads the retryable CLASS, never the error text.
func TestRateLimitExhaustionIsProviderExhausted(t *testing.T) {
	mgr, root, childID := spawnFailedChild(t,
		provider.MarkRetryable(errors.New("anthropic: rate limited"), provider.RetryableRateLimited))

	node, _, err := mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo: %v", err)
	}
	if node.FailKind != FailKindProviderExhausted {
		t.Errorf("FailKind = %q, want %q", node.FailKind, FailKindProviderExhausted)
	}
}

// TestOverloadIsNotProviderExhausted is the companion guard: overload and
// 5xx weather stay ordinary failures. A sibling may well succeed against
// an overloaded provider, so the do-not-respawn guidance must not fire.
func TestOverloadIsNotProviderExhausted(t *testing.T) {
	mgr, root, childID := spawnFailedChild(t,
		provider.MarkRetryable(errors.New("anthropic: overloaded"), provider.RetryableOverloaded))

	node, _, err := mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo: %v", err)
	}
	if node.FailKind != "" {
		t.Errorf("FailKind = %q, want empty for an overload", node.FailKind)
	}
}

// TestProviderExhaustedNotificationTellsParentNotToRespawn is the
// parent-facing contract: the model-visible notification must say the
// child is preserved, that a replacement is pointless, and how to resume.
func TestProviderExhaustedNotificationTellsParentNotToRespawn(t *testing.T) {
	_, root, childID := spawnFailedChild(t, exhaustionError("2026-09-01"))

	seg := root.checkoutTaskNotificationsSegment()
	for _, want := range []string{
		childID,
		"provider exhausted",
		"do not spawn a replacement",
		"task send",
		"2026-09-01",
	} {
		if !strings.Contains(seg, want) {
			t.Errorf("notification segment omits %q:\n%s", want, seg)
		}
	}
	// One line per notification, still — the guidance must not open a
	// forgery surface by adding lines (renderTaskNotifications' rule).
	if got := strings.Count(strings.TrimSuffix(strings.TrimPrefix(seg, "[tasks:"), "\n]"), "\n"); got != 1 {
		t.Errorf("notification block has %d newlines inside, want exactly 1 (one line for one child)\n%s", got, seg)
	}
}

// TestOrdinaryFailureNotificationHasNoResumeGuidance is the surplus guard
// for the render: an ordinary failure must not tell a parent to wait.
func TestOrdinaryFailureNotificationHasNoResumeGuidance(t *testing.T) {
	_, root, _ := spawnFailedChild(t, errors.New("tool crashed"))

	seg := root.checkoutTaskNotificationsSegment()
	if strings.Contains(seg, "provider exhausted") {
		t.Errorf("ordinary failure notification claims exhaustion:\n%s", seg)
	}
}

// TestTaskStatusReportsFailKind proves the model can read the structured
// kind back through the tool it already uses to inspect a descendant.
func TestTaskStatusReportsFailKind(t *testing.T) {
	mgr, root, childID := spawnFailedChild(t, exhaustionError("2026-09-01"))
	_ = mgr

	parts, err := runTaskStatus(root, taskToolArgs{SessionID: childID})
	if err != nil {
		t.Fatalf("runTaskStatus: %v", err)
	}
	var got taskStatusResult
	if err := json.Unmarshal([]byte(parts.Text()), &got); err != nil {
		t.Fatalf("unmarshal status result %q: %v", parts.Text(), err)
	}
	if got.FailKind != FailKindProviderExhausted {
		t.Errorf("fail_kind = %q, want %q", got.FailKind, FailKindProviderExhausted)
	}
	if got.Status != string(StatusFailed) {
		t.Errorf("status = %q, want %q", got.Status, StatusFailed)
	}
}

// recoverAfterExhaustionProvider fails its first Stream call with an
// account-wall error and answers normally afterwards — the provider
// recovering while the failed child still sits, preserved, in the tree.
type recoverAfterExhaustionProvider struct {
	name  string
	calls int
}

func (p *recoverAfterExhaustionProvider) Name() string { return p.name }

func (p *recoverAfterExhaustionProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	p.calls++
	if p.calls == 1 {
		return nil, exhaustionError("2026-09-01")
	}
	msg := &message.Message{ID: "msg_resumed", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "resumed and finished"}}}
	return &scriptedStream{events: []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}}, nil
}

// TestProviderExhaustedChildResumesOnSend proves the guidance the
// notification gives is actually true: the SAME child, already settled
// failed on an account wall, re-runs cleanly through the existing
// send-to-a-settled-descendant path once the provider recovers, and its
// history from the first attempt is still there.
func TestProviderExhaustedChildResumesOnSend(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	prov := &recoverAfterExhaustionProvider{name: "child"}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), prov))

	childID, err := mgr.Spawn(SpawnOptions{
		ParentID:  root.ID,
		Prompt:    "do the work",
		Model:     modelFor("child"),
		AgentType: "general-purpose",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusFailed, time.Second)

	node, _, err := mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo: %v", err)
	}
	if node.FailKind != FailKindProviderExhausted {
		t.Fatalf("test setup: FailKind = %q, want %q", node.FailKind, FailKindProviderExhausted)
	}
	// Drain the failure notification so the assertion below reads the
	// RESUMED turn's own notification, not the first one.
	root.checkoutTaskNotificationsSegment()
	root.commitTaskNotifications()

	queued, err := mgr.SendToDescendant(root.ID, childID, "the provider recovered; continue where you left off")
	if err != nil {
		t.Fatalf("SendToDescendant on a provider-exhausted child: %v", err)
	}
	if queued {
		t.Fatal("SendToDescendant queued = true, want false: a settled child is re-run, not queued")
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	node, _, err = mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo after resume: %v", err)
	}
	if node.Result != "resumed and finished" {
		t.Errorf("Result = %q, want the resumed turn's answer", node.Result)
	}
	if node.FailKind != "" || node.FailReason != "" {
		t.Errorf("FailKind = %q, FailReason = %q, want both cleared by the successful resume", node.FailKind, node.FailReason)
	}

	// The child's own first attempt is still in its history: "preserved"
	// in the notification means the work, not just the session id.
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session(child): not found")
	}
	var firstPrompt bool
	for _, m := range child.History() {
		if m.Role == message.RoleUser && strings.Contains(m.Parts.Text(), "do the work") {
			firstPrompt = true
		}
	}
	if !firstPrompt {
		t.Error("child history lost the original prompt across the resume")
	}
}

// TestProviderExhaustedOutcomeSurvivesReload proves the classification is
// durable: a parent that reads a reloaded child's outcome after a process
// restart still learns not to respawn.
func TestProviderExhaustedOutcomeSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	reg := provider.Registry{"root": scriptedTurns("root", nil)}
	parent := NewSession(Config{Providers: reg, Model: modelFor("root"), SessionDir: dir})
	parent.enqueueTaskNotification(taskNotification{
		ChildID:    "ses_child",
		Agent:      "general-purpose",
		Status:     StatusFailed,
		FailKind:   FailKindProviderExhausted,
		FailReason: "provider usage limit reached for this account; access returns 2026-09-01",
	})

	reloaded, err := LoadSession(Config{Providers: reg, SessionDir: dir}, parent.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	reloaded.mu.Lock()
	pending := append([]taskNotification(nil), reloaded.taskNotifications...)
	reloaded.mu.Unlock()
	if len(pending) != 1 {
		t.Fatalf("restored %d pending notifications, want 1", len(pending))
	}
	if pending[0].FailKind != FailKindProviderExhausted {
		t.Errorf("restored FailKind = %q, want %q", pending[0].FailKind, FailKindProviderExhausted)
	}
}
