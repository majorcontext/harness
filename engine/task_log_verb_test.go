// Tests for the `task` tool's log verb: a parent reading the tail of a
// descendant's transcript, living or dead.
//
// Incident this serves: a child died and its parent had only a one-line
// fail reason to reason from. The child's own last messages — the tool it
// was running, what it had already found — were sitting in a session log
// the parent had no in-process way to read.
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

// logFailingProvider fails every Stream call with err — a child that dies
// on its very first turn.
type logFailingProvider struct {
	name string
	err  error
}

func (p *logFailingProvider) Name() string { return p.name }

func (p *logFailingProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	return nil, p.err
}

// runTaskLogJSON runs the log verb on behalf of caller and unmarshals its
// result.
func runTaskLogJSON(t *testing.T, caller *Session, in taskToolArgs) taskLogResult {
	t.Helper()
	parts, err := runTaskLog(caller, in)
	if err != nil {
		t.Fatalf("runTaskLog(%+v): %v", in, err)
	}
	var got taskLogResult
	if err := json.Unmarshal([]byte(parts.Text()), &got); err != nil {
		t.Fatalf("unmarshal log result %q: %v", parts.Text(), err)
	}
	return got
}

// spawnLoggedChild spawns one child that answers with text and settles
// done, returning the manager, root, and child id.
func spawnLoggedChild(t *testing.T, answer string) (*SessionManager, *Session, string) {
	t.Helper()
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn(answer)),
	))
	childID, err := mgr.Spawn(SpawnOptions{
		ParentID:  root.ID,
		Prompt:    "count the files",
		Model:     modelFor("child"),
		AgentType: "explore",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	return mgr, root, childID
}

// TestTaskLogReturnsDescendantTail proves the verb answers with the
// child's own messages, newest last, plus the lifecycle facts a reader
// needs to interpret them.
func TestTaskLogReturnsDescendantTail(t *testing.T) {
	_, root, childID := spawnLoggedChild(t, "there are 42 files")

	got := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID})
	if got.SessionID != childID {
		t.Errorf("session_id = %q, want %q", got.SessionID, childID)
	}
	if got.Status != string(StatusDone) {
		t.Errorf("status = %q, want %q", got.Status, StatusDone)
	}
	if got.Total != 2 || got.Returned != 2 {
		t.Errorf("total = %d, returned = %d, want 2 and 2 (the prompt and the answer)", got.Total, got.Returned)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(got.Entries), got.Entries)
	}
	if got.Entries[0].Role != string(message.RoleUser) || !strings.Contains(got.Entries[0].Text, "count the files") {
		t.Errorf("entries[0] = %+v, want the child's own prompt", got.Entries[0])
	}
	if got.Entries[1].Role != string(message.RoleAssistant) || !strings.Contains(got.Entries[1].Text, "42 files") {
		t.Errorf("entries[1] = %+v, want the child's final answer last", got.Entries[1])
	}
}

// TestTaskLogReadsADeadChild is the verb's whole reason to exist: a child
// that already failed must still be readable, with its final messages and
// the reason it died.
func TestTaskLogReadsADeadChild(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		&logFailingProvider{name: "child", err: provider.MarkPermanent(errors.New("anthropic: the wall"))},
	))
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

	got := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID})
	if got.Status != string(StatusFailed) {
		t.Errorf("status = %q, want %q", got.Status, StatusFailed)
	}
	if got.FailReason == "" {
		t.Error("fail_reason is empty, want the child's own failure reason alongside its tail")
	}
	if got.Returned == 0 {
		t.Fatalf("returned = 0, want the dead child's messages: %+v", got)
	}
	if !strings.Contains(got.Entries[len(got.Entries)-1].Text, "do the work") {
		t.Errorf("last entry = %+v, want the prompt the child died on", got.Entries[len(got.Entries)-1])
	}
}

// TestTaskLogRendersToolCallsAndResults proves the tail is legible for the
// case that matters most — a child that died mid-tool-loop — rather than
// silently dropping every non-text part.
func TestTaskLogRendersToolCallsAndResults(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", nil)))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusFailed, time.Second)

	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session(child): not found")
	}
	child.append(message.Message{ID: "m_call", Role: message.RoleAssistant, Parts: message.Parts{
		&message.Text{Text: "checking"},
		toolCall("tc1", "bash", `{"command":"ls -1"}`),
	}})
	child.append(message.Message{ID: "m_res", Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "tc1", Content: message.Parts{&message.Text{Text: "a.go\nb.go"}}},
	}})

	got := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID})
	last := got.Entries[len(got.Entries)-1]
	if !strings.Contains(last.Text, "a.go") {
		t.Errorf("tool-result entry = %+v, want the tool output", last)
	}
	call := got.Entries[len(got.Entries)-2]
	if !strings.Contains(call.Text, "bash") || !strings.Contains(call.Text, "ls -1") {
		t.Errorf("tool-call entry = %+v, want the tool name and its arguments", call)
	}
}

// TestTaskLogTailBounds pins the three tail answers: the default, an
// explicit request, and the clamp that stops one call from pulling a whole
// long-running child's transcript into the parent's context.
func TestTaskLogTailBounds(t *testing.T) {
	mgr, root, childID := spawnLoggedChild(t, "done")
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session(child): not found")
	}
	for i := 0; i < taskLogMaxTail+taskLogDefaultTail; i++ {
		child.append(message.Message{ID: "m", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "filler"}}})
	}
	total := len(child.History())

	def := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID})
	if def.Returned != taskLogDefaultTail {
		t.Errorf("default returned = %d, want %d", def.Returned, taskLogDefaultTail)
	}
	if def.Total != total {
		t.Errorf("total = %d, want %d (the whole transcript, so the model knows what it did not get)", def.Total, total)
	}

	few := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID, Tail: 3})
	if few.Returned != 3 {
		t.Errorf("tail=3 returned = %d, want 3", few.Returned)
	}

	clamped := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID, Tail: taskLogMaxTail * 10})
	if clamped.Returned > taskLogMaxTail {
		t.Errorf("tail=%d returned = %d, want it clamped to %d", taskLogMaxTail*10, clamped.Returned, taskLogMaxTail)
	}
}

// TestTaskLogRejectsNegativeTail proves a nonsense tail is an error, not a
// silent reinterpretation: a model that asked for -5 gets told, rather
// than quietly handed the default.
func TestTaskLogRejectsNegativeTail(t *testing.T) {
	_, root, childID := spawnLoggedChild(t, "done")
	if _, err := runTaskLog(root, taskToolArgs{SessionID: childID, Tail: -5}); err == nil {
		t.Error("runTaskLog(tail=-5) = nil error, want a rejection")
	}
}

// TestTaskLogBoundsTotalSize proves one call cannot flood the parent's
// context: entries are filled newest-first under a total budget, and the
// count reported says how many actually came back.
func TestTaskLogBoundsTotalSize(t *testing.T) {
	mgr, root, childID := spawnLoggedChild(t, "done")
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session(child): not found")
	}
	big := strings.Repeat("x", taskLogEntryCap*2)
	for i := 0; i < taskLogDefaultTail; i++ {
		child.append(message.Message{ID: "m", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: big}}})
	}

	got := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID})
	if got.Returned >= taskLogDefaultTail {
		t.Errorf("returned = %d, want fewer than the %d requested (the total budget must bite)", got.Returned, taskLogDefaultTail)
	}
	size := 0
	for _, e := range got.Entries {
		size += len([]rune(e.Text))
		if len([]rune(e.Text)) > taskLogEntryCap+len(taskLogTruncationMarker) {
			t.Errorf("entry of %d runes exceeds the %d-rune per-entry cap", len([]rune(e.Text)), taskLogEntryCap)
		}
	}
	if size > taskLogTotalCap+taskLogEntryCap {
		t.Errorf("total rendered size = %d runes, want it bounded near %d", size, taskLogTotalCap)
	}
	if !got.Entries[0].Truncated {
		t.Error("an entry cut at the per-entry cap must be marked truncated")
	}
	// Newest-first filling: the entries kept are the LAST ones, which is
	// what a reader diagnosing a death needs.
	if got.Entries[len(got.Entries)-1].Text[:1] != "x" {
		t.Errorf("last entry = %q..., want the newest message", got.Entries[len(got.Entries)-1].Text[:10])
	}
}

// TestTaskLogAncestorGate proves the verb obeys the same rule cancel,
// status, and send obey: a session may inspect what it spawned,
// transitively, and nothing else.
func TestTaskLogAncestorGate(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("child done")),
	))
	other := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if _, err := runTaskLog(other, taskToolArgs{SessionID: childID}); err == nil {
		t.Error("an unrelated session read a child's transcript, want a refusal")
	} else if !strings.Contains(err.Error(), "not a session you spawned") {
		t.Errorf("unrelated-session error = %v, want the not-a-descendant refusal", err)
	}

	if _, err := runTaskLog(root, taskToolArgs{SessionID: root.ID}); err == nil {
		t.Error("a session read its OWN transcript through the descendant gate, want a refusal")
	}

	if _, err := runTaskLog(root, taskToolArgs{SessionID: "ses_nope"}); err == nil {
		t.Error("an unknown session id was accepted, want a refusal")
	} else if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("unknown-id error = %v, want the no-such-session refusal", err)
	}

	if _, err := runTaskLog(root, taskToolArgs{}); err == nil {
		t.Error("a missing session_id was accepted, want a refusal")
	}
}

// TestDescendantTranscriptReachesGrandchildren proves the gate is
// transitive at the SessionManager level, matching DescendantInfo.
func TestDescendantTranscriptReachesGrandchildren(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("child done")),
		scriptedTurns("grand", doneTurn("grand done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn child: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	grandID, err := mgr.Spawn(SpawnOptions{ParentID: childID, Prompt: "deeper", Model: modelFor("grand")})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)

	node, msgs, total, err := mgr.DescendantTranscript(root.ID, grandID, taskLogDefaultTail)
	if err != nil {
		t.Fatalf("DescendantTranscript(root, grandchild): %v", err)
	}
	if node.ID != grandID {
		t.Errorf("node.ID = %q, want %q", node.ID, grandID)
	}
	if len(msgs) == 0 {
		t.Error("grandchild transcript is empty, want its messages")
	}
	if total != len(msgs) {
		t.Errorf("total = %d, want %d (nothing was trimmed at this size)", total, len(msgs))
	}
	if _, _, _, err := mgr.DescendantTranscript(grandID, root.ID, taskLogDefaultTail); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("DescendantTranscript(grandchild, root) err = %v, want ErrNotDescendant", err)
	}
}

// TestTaskLogRoutesThroughTheToolDispatch proves the verb is reachable the
// way a model reaches it — through runTaskTool's action dispatch — and
// that the tool's own schema advertises it.
func TestTaskLogRoutesThroughTheToolDispatch(t *testing.T) {
	_, root, childID := spawnLoggedChild(t, "the answer")

	raw := json.RawMessage(`{"action":"log","session_id":"` + childID + `","tail":1}`)
	parts, err := runTaskTool(root, raw)
	if err != nil {
		t.Fatalf("runTaskTool(log): %v", err)
	}
	var got taskLogResult
	if err := json.Unmarshal([]byte(parts.Text()), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Returned != 1 || !strings.Contains(got.Entries[0].Text, "the answer") {
		t.Errorf("log via dispatch = %+v, want the single newest entry", got)
	}

	def := taskTool().Def
	if !strings.Contains(string(def.InputSchema), `"log"`) {
		t.Error("task tool schema does not offer the log action")
	}
	if !strings.Contains(def.Description, "log(") {
		t.Error("task tool description does not document the log action")
	}
	if !strings.Contains(string(def.InputSchema), `"tail"`) {
		t.Error("task tool schema does not offer the tail property")
	}
}

// TestTaskLogSurfacesToolResultAttachments proves an image a tool returned
// is visible in the tail. Parts.Text() renders Text parts only, so a
// read_file/MCP [Text, Blob] result would otherwise read as a one-line
// summary with no sign that a picture came back — a review finding.
func TestTaskLogSurfacesToolResultAttachments(t *testing.T) {
	mgr, root, childID := spawnLoggedChild(t, "done")
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session(child): not found")
	}
	child.append(message.Message{ID: "m_img", Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "tc1", Content: message.Parts{
			&message.Text{Text: "PNG 800x600, 12345 bytes"},
			&message.Blob{MediaType: "image/png", Data: []byte{1, 2, 3}},
		}},
	}})

	got := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID})
	last := got.Entries[len(got.Entries)-1]
	if !strings.Contains(last.Text, "1 attachment(s)") {
		t.Errorf("tool-result entry = %q, want the attachment counted", last.Text)
	}
}

// TestTaskLogSkipsEmptyTextParts proves an empty Text part contributes no
// blank line — it spends budget and tells a reader less than no line.
func TestTaskLogSkipsEmptyTextParts(t *testing.T) {
	mgr, root, childID := spawnLoggedChild(t, "done")
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session(child): not found")
	}
	// An empty part on EITHER side of the real one: a leading empty part
	// is absorbed by the builder's own first-line handling, but a
	// trailing one used to append a bare newline.
	child.append(message.Message{ID: "m_mixed", Role: message.RoleAssistant, Parts: message.Parts{
		&message.Text{Text: ""},
		&message.Text{Text: "real content"},
		&message.Text{Text: ""},
	}})

	got := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID})
	if last := got.Entries[len(got.Entries)-1]; last.Text != "real content" {
		t.Errorf("entry text = %q, want %q with no blank line on either side", last.Text, "real content")
	}
}

// TestTaskLogTruncatedCoversInnerCuts proves the structured flag reports
// the whole truth. A review finding: Truncated reflected only the
// entry-level cap, so a tool call whose 5000-rune arguments were cut to
// 300 — inside an entry whose total text stayed well under the entry cap
// — reported Truncated: false, and a reader keying on the field read a
// cut entry as complete.
func TestTaskLogTruncatedCoversInnerCuts(t *testing.T) {
	mgr, root, childID := spawnLoggedChild(t, "done")
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session(child): not found")
	}
	args := `{"command":"` + strings.Repeat("z", taskLogArgsCap*3) + `"}`
	child.append(message.Message{ID: "m_args", Role: message.RoleAssistant, Parts: message.Parts{
		toolCall("tc1", "bash", args),
	}})

	got := runTaskLogJSON(t, root, taskToolArgs{SessionID: childID, Tail: 1})
	entry := got.Entries[0]
	if len([]rune(entry.Text)) >= taskLogEntryCap {
		t.Fatalf("test setup: entry is %d runes, want it under the %d-rune entry cap so only the INNER cut can set the flag",
			len([]rune(entry.Text)), taskLogEntryCap)
	}
	if !entry.Truncated {
		t.Errorf("entry = %+v, want Truncated: true from the capped tool-call arguments", entry)
	}
}

// TestTaskLogBoundsToolResultTextAtThePart proves a huge tool result is
// cut where it is read, not copied whole into the builder and cut after —
// a review finding: a mid-loop child can hold a 200KB read_file result,
// and a tail of many such messages would allocate tens of MB to discard
// almost all of it.
func TestTaskLogBoundsToolResultTextAtThePart(t *testing.T) {
	huge := strings.Repeat("q", taskLogEntryCap*10)
	got, cut := boundedPartsText(message.Parts{&message.Text{Text: huge}}, taskLogEntryCap)
	if !cut {
		t.Error("boundedPartsText cut = false, want true")
	}
	if len([]rune(got)) > taskLogEntryCap+len(taskLogTruncationMarker) {
		t.Errorf("boundedPartsText returned %d runes, want it bounded at %d", len([]rune(got)), taskLogEntryCap)
	}
	if !strings.HasSuffix(got, taskLogTruncationMarker) {
		t.Errorf("boundedPartsText = %q..., want the truncation marker", got[:40])
	}

	// It renders like Parts.Text() when nothing is cut: Text parts only,
	// newline-joined, so the tail reads the same as before.
	ps := message.Parts{&message.Text{Text: "a"}, &message.Blob{MediaType: "image/png"}, &message.Text{Text: "b"}}
	if got, cut := boundedPartsText(ps, taskLogEntryCap); got != ps.Text() || cut {
		t.Errorf("boundedPartsText = (%q, %v), want (%q, false) — identical to Parts.Text() under the cap", got, cut, ps.Text())
	}
}
