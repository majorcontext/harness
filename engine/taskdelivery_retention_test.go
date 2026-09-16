package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// handleFromSegment pulls the trh_N token out of a rendered notification's
// read_tool_result(handle="trh_N") clause.
func handleFromSegment(t *testing.T, seg string) string {
	t.Helper()
	const key = `handle="`
	i := strings.Index(seg, key)
	if i < 0 {
		t.Fatalf("no handle clause in segment:\n%s", seg)
	}
	rest := seg[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("unterminated handle in segment:\n%s", seg)
	}
	return rest[:j]
}

// TestCheckoutRetainsOversizedDoneResult: a subagent whose final result
// exceeds the pinned-notification budget is retained into the parent's
// tool-result store, so the [tasks:] line carries a bounded preview plus a
// read_tool_result handle instead of the inert "… [truncated]" dead end, and
// the parent reads the whole report back through the ordinary tool.
func TestCheckoutRetainsOversizedDoneResult(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 100})
	report := strings.Repeat("seam map line with enough text to matter\n", 500)
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_child", Agent: "explore", Status: StatusDone, Result: report})

	seg := s.checkoutTaskNotificationsSegment()

	if strings.Contains(seg, report) {
		t.Fatal("notification carried the full report inline; want a bounded preview")
	}
	if !strings.Contains(seg, "read_tool_result(handle=") {
		t.Fatalf("notification does not name the retrieval tool:\n%s", seg)
	}
	if strings.Contains(seg, taskLogTruncationMarker) {
		t.Errorf("notification used the inert truncation marker instead of a handle:\n%s", seg)
	}

	handle := handleFromSegment(t, seg)
	parts, err := runReadToolResult(s, json.RawMessage(`{"handle":"`+handle+`","max_bytes":65536}`))
	if err != nil {
		t.Fatalf("read_tool_result(%s): %v", handle, err)
	}
	if got := partsText(parts); !strings.Contains(got, "seam map line with enough text to matter") {
		t.Fatalf("read_tool_result did not return the retained report; got %d bytes", len(got))
	}
}

// TestCheckoutRetainsOnceAcrossRetries: a retried turn re-renders the same
// in-flight notification, and must reuse the one minted handle rather than
// minting a fresh trh_N (and a second sidecar file) every render.
func TestCheckoutRetainsOnceAcrossRetries(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 100})
	report := strings.Repeat("line\n", 500)
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_child", Status: StatusDone, Result: report})

	a := s.checkoutTaskNotificationsSegment()
	b := s.checkoutTaskNotificationsSegment()
	if a != b {
		t.Fatalf("retry rendered differently:\n a=%s\n b=%s", a, b)
	}
	if _, ok := s.lookupToolResult(toolResultHandlePrefix + "2"); ok {
		t.Error("a second handle was minted across retries")
	}
}

// TestCheckoutFallsBackWhenRetentionDisabled: with no store (retention off),
// an oversized result keeps the existing truncate-and-mark behavior, never a
// dangling handle that names bytes no store holds.
func TestCheckoutFallsBackWhenRetentionDisabled(t *testing.T) {
	s := NewSession(Config{WorkDir: t.TempDir()})
	report := strings.Repeat("line\n", 1000)
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_child", Status: StatusDone, Result: report})

	seg := s.checkoutTaskNotificationsSegment()
	if strings.Contains(seg, "read_tool_result(handle=") {
		t.Errorf("named a handle with retention disabled:\n%s", seg)
	}
	if !strings.Contains(seg, taskLogTruncationMarker) {
		t.Errorf("did not fall back to the truncation marker:\n%s", seg)
	}
}

// A secret must not reach the parent even when retention is refused (here by
// a tiny ceiling): the fallback preview is masked, not the raw result.
func TestNotificationMasksSecretOnNoHandleFallback(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 100, ToolResultRetainedBytes: 1})
	report := "TOKEN=supersecretvalue123 " + strings.Repeat("x", 200)
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_c", Status: StatusDone, Result: report})

	seg := s.checkoutTaskNotificationsSegment()
	if strings.Contains(seg, "supersecretvalue123") {
		t.Fatalf("secret leaked into notification:\n%s", seg)
	}
	if !strings.Contains(seg, taskResultUnrecoverableMarker) {
		t.Errorf("truncated no-handle preview not marked unrecoverable:\n%s", seg)
	}
}

// A re-run producing the identical result must be retained once, not write a
// second sidecar whose handle is then discarded.
func TestNotificationIdenticalResultRetainedOnce(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 100})
	report := strings.Repeat("q", 300)
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_c", Status: StatusDone, Result: report})
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_c", Status: StatusDone, Result: report})

	s.checkoutTaskNotificationsSegment()
	if _, ok := s.lookupToolResult(toolResultHandlePrefix + "2"); ok {
		t.Error("a second sidecar was written for an identical rerun result")
	}
}

// A Claude Code delegated parent has read_tool_result in its registry but
// never dispatches it, so it must get a marked preview, not a dead handle.
func TestNotificationNoHandleForDelegatedParent(t *testing.T) {
	s := NewSession(Config{
		SessionDir:            t.TempDir(),
		ToolResultInlineBytes: 100,
		Model:                 message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
	})
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_c", Status: StatusDone, Result: strings.Repeat("z", 300)})

	seg := s.checkoutTaskNotificationsSegment()
	if strings.Contains(seg, "read_tool_result(handle=") {
		t.Errorf("minted a handle a delegated parent cannot call:\n%s", seg)
	}
	if !strings.Contains(seg, taskResultUnrecoverableMarker) {
		t.Errorf("delegated truncation not marked unrecoverable:\n%s", seg)
	}
}

// With retention on, a result past the 4000-rune legacy cap but within the
// byte budget renders in full, never the inert "… [truncated]" marker.
func TestNotificationNoInertMarkerAboveRuneCap(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 16384})
	report := strings.Repeat("a", 4050)
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_c", Status: StatusDone, Result: report})

	seg := s.checkoutTaskNotificationsSegment()
	if strings.Contains(seg, taskLogTruncationMarker) {
		t.Errorf("inert marker used in the byte/rune gap")
	}
	if !strings.Contains(seg, report) {
		t.Error("result was truncated instead of rendered in full")
	}
}

// A re-run child produces a second notification with different text; each
// must get its own handle, not the first result's.
func TestNotificationMemoDistinguishesResultsPerChild(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 100})
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_c", Status: StatusDone, Result: "AAAA" + strings.Repeat("1", 300)})
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_c", Status: StatusDone, Result: "BBBB" + strings.Repeat("2", 300)})

	seg := s.checkoutTaskNotificationsSegment()
	if !strings.Contains(seg, toolResultHandlePrefix+"1") || !strings.Contains(seg, toolResultHandlePrefix+"2") {
		t.Fatalf("two results of one child did not get distinct handles:\n%s", seg)
	}
	if !strings.Contains(seg, "AAAA") || !strings.Contains(seg, "BBBB") {
		t.Fatalf("a result rendered under the other's preview:\n%s", seg)
	}
}

// A parent whose agent def omits read_tool_result must not be handed a handle
// it cannot act on.
func TestNotificationNoHandleWhenParentLacksReadTool(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 100})
	delete(s.tools, readToolResultToolName)
	s.enqueueTaskNotification(taskNotification{ChildID: "ses_c", Status: StatusDone, Result: strings.Repeat("z", 300)})

	seg := s.checkoutTaskNotificationsSegment()
	if strings.Contains(seg, "read_tool_result(handle=") {
		t.Errorf("emitted a handle the parent cannot read:\n%s", seg)
	}
	if strings.Contains(seg, taskLogTruncationMarker) {
		t.Errorf("inert marker despite retention active:\n%s", seg)
	}
}
