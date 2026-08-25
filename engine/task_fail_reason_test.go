// Tests for the cause-carrying half of a failed child's fail_reason:
// classifySpawnError (session_manager.go) appends the underlying provider
// error to its classified prefix, so a parent learns WHY a child died, not
// only that it did.
//
// Incident this guards: a child died on "[permanent] anthropic: You have
// reached your specified API usage limits...", the parent's notification
// read only "turn failed and did not recover", and the parent respawned a
// sibling straight into the same fleet-wide wall.
package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/provider"
)

// usageLimitDetail is the live incident's own provider message, used
// verbatim as the distinctive string every assertion below looks for.
const usageLimitDetail = "You have reached your specified API usage limits. You will regain access on 2026-09-01"

// failingProvider fails every Stream call with err — the shape a child
// takes when the provider itself rejects the turn.
type failingProvider struct {
	name string
	err  error
}

func (p *failingProvider) Name() string { return p.name }

func (p *failingProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	return nil, p.err
}

// spawnFailedChild spawns one child whose provider fails with err, waits
// for it to settle StatusFailed, and returns the manager, the root, and
// the child id.
func spawnFailedChild(t *testing.T, err error) (*SessionManager, *Session, string) {
	t.Helper()
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		&failingProvider{name: "child", err: err},
	))
	childID, spawnErr := mgr.Spawn(SpawnOptions{
		ParentID:  root.ID,
		Prompt:    "do the work",
		Model:     modelFor("child"),
		AgentType: "general-purpose",
	})
	if spawnErr != nil {
		t.Fatalf("Spawn: %v", spawnErr)
	}
	waitForStatus(t, mgr, childID, StatusFailed, time.Second)
	return mgr, root, childID
}

// TestSpawnFailureNotificationCarriesProviderCause proves the parent's
// completion notification names the real cause. Before this fix the
// rendered [tasks: ...] line said only "turn failed and did not recover".
func TestSpawnFailureNotificationCarriesProviderCause(t *testing.T) {
	_, root, childID := spawnFailedChild(t, provider.MarkPermanent(errors.New("anthropic: "+usageLimitDetail)))

	seg := root.checkoutTaskNotificationsSegment()
	if !strings.Contains(seg, childID) {
		t.Fatalf("notification segment does not mention child %s:\n%s", childID, seg)
	}
	if !strings.Contains(seg, usageLimitDetail) {
		t.Errorf("notification segment omits the provider cause %q:\n%s", usageLimitDetail, seg)
	}
	if !strings.Contains(seg, "[permanent]") {
		t.Errorf("notification segment omits the permanent marker:\n%s", seg)
	}
}

// TestDescendantInfoFailReasonCarriesProviderCause proves the engine-level
// counterpart of the wire's session.info payload — the same snapshot
// GET /session/{id}.lineage.fail_reason is built from — carries the cause
// too, so a parent that reads status after the fact sees what the
// notification said.
func TestDescendantInfoFailReasonCarriesProviderCause(t *testing.T) {
	mgr, root, childID := spawnFailedChild(t, provider.MarkPermanent(errors.New("anthropic: "+usageLimitDetail)))

	node, _, err := mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo: %v", err)
	}
	if !strings.Contains(node.FailReason, usageLimitDetail) {
		t.Errorf("DescendantInfo fail_reason = %q, want it to contain %q", node.FailReason, usageLimitDetail)
	}

	info, ok := mgr.Info(childID)
	if !ok {
		t.Fatalf("Info(%s): not found", childID)
	}
	if info.FailReason != node.FailReason {
		t.Errorf("Info fail_reason = %q, DescendantInfo fail_reason = %q, want identical", info.FailReason, node.FailReason)
	}
}

// TestClassifySpawnErrorKeepsClassifiedPrefix pins the shape of every
// classified reason: the prefix that already existed, then the cause.
func TestClassifySpawnErrorKeepsClassifiedPrefix(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantPrefix string
		wantDetail string
	}{
		{
			name:       "permanent",
			err:        provider.MarkPermanent(errors.New("anthropic: bad request")),
			wantPrefix: "turn failed with a permanent provider error and cannot succeed on retry: ",
			wantDetail: "[permanent] anthropic: bad request",
		},
		{
			name:       "retryable exhausted",
			err:        provider.MarkRetryable(errors.New("anthropic: overloaded"), provider.RetryableOverloaded),
			wantPrefix: "provider overloaded errors exhausted the retry budget: ",
			wantDetail: "anthropic: overloaded",
		},
		{
			name:       "deterministic",
			err:        errors.New("engine: unexpected EOF from stream"),
			wantPrefix: "turn failed and did not recover: ",
			wantDetail: "engine: unexpected EOF from stream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySpawnError(tc.err)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("classifySpawnError = %q, want prefix %q", got, tc.wantPrefix)
			}
			if !strings.Contains(got, tc.wantDetail) {
				t.Errorf("classifySpawnError = %q, want it to contain %q", got, tc.wantDetail)
			}
		})
	}
}

// TestClassifySpawnErrorContextCausesStayFixed proves a canceled or
// timed-out turn keeps its short fixed reason: "context canceled" adds
// nothing a parent can act on, and both strings are compared elsewhere.
func TestClassifySpawnErrorContextCausesStayFixed(t *testing.T) {
	if got := classifySpawnError(context.Canceled); got != "canceled" {
		t.Errorf("classifySpawnError(context.Canceled) = %q, want %q", got, "canceled")
	}
	if got := classifySpawnError(context.DeadlineExceeded); got != "timed out" {
		t.Errorf("classifySpawnError(context.DeadlineExceeded) = %q, want %q", got, "timed out")
	}
	if got := classifySpawnError(nil); got != "" {
		t.Errorf("classifySpawnError(nil) = %q, want empty", got)
	}
}

// TestClassifySpawnErrorTruncatesLongCause proves a provider error that
// embeds a whole response body cannot balloon the notification a parent
// replays on every later turn.
func TestClassifySpawnErrorTruncatesLongCause(t *testing.T) {
	long := strings.Repeat("x", spawnErrorDetailCap*3)
	got := classifySpawnError(errors.New(long))
	if len([]rune(got)) > spawnErrorDetailCap+len(spawnErrorDetailTruncationMarker)+120 {
		t.Errorf("classifySpawnError length = %d runes, want it bounded near the %d-rune cap", len([]rune(got)), spawnErrorDetailCap)
	}
	if !strings.Contains(got, spawnErrorDetailTruncationMarker) {
		t.Errorf("classifySpawnError = %q, want the truncation marker %q", got, spawnErrorDetailTruncationMarker)
	}
}

// TestClassifySpawnErrorMasksSecretsInCause proves the cause runs through
// the same masking every other model-visible text uses: a provider error
// can quote the request it rejected, headers included.
func TestClassifySpawnErrorMasksSecretsInCause(t *testing.T) {
	got := classifySpawnError(errors.New(`openai: 401 on request (Authorization: Bearer sk-live-abcdefgh12345678)`))
	if strings.Contains(got, "sk-live-abcdefgh12345678") {
		t.Errorf("classifySpawnError = %q, want the bearer token masked", got)
	}
	if !strings.Contains(got, "401") {
		t.Errorf("classifySpawnError = %q, want the rest of the cause preserved", got)
	}
}
