package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// engineContextCaptureProv records the concatenated text of every
// *message.EngineContext part seen across every Stream call — see
// message.EngineContext's own doc comment for why the ambient [tasks:]
// notification segment (engine/process.go's withAmbientStatus,
// engine/taskdelivery.go's renderTaskNotifications) is carried as this
// distinct, typed part, never as plain *message.Text: a capture that only
// concatenates *Text parts (like queue_test.go's orderCaptureProv) is
// blind to it by construction, since provider.Request carries the
// CANONICAL message structure, not pre-transcoded wire text.
type engineContextCaptureProv struct {
	name string
	mu   sync.Mutex

	blocks []string
}

func (p *engineContextCaptureProv) Name() string { return p.name }

func (p *engineContextCaptureProv) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	for _, m := range req.Messages {
		for _, part := range m.Parts {
			if ec, ok := part.(*message.EngineContext); ok {
				p.blocks = append(p.blocks, ec.Text)
			}
		}
	}
	p.mu.Unlock()
	msg := &message.Message{ID: "msg_a", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "root turn after restart"}}}
	return &scriptedStream{events: []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}}, nil
}

// TestRestartRecoveryEngagesWithoutTouchingCrashedChildDirectly is the
// end-to-end regression test for a live prod finding on a
// restartPolicy:Always box (harness serve as PID 1, kill -9 1 mid-child):
// after restart, (1) the crashed child's wire lineage still rendered
// correctly from disk on a plain, read-only GET — that half already
// worked — but (2) the root NEVER received a lost-to-restart notification
// for it, because nothing in the box's own traffic ever independently
// touched the crashed child's own id again — only the root, via later
// reads and an eventual follow-up turn.
//
// Reproduces the EXACT sequence: spawn parent+child, block the child
// mid-turn (simulating the kill), build a second, fully independent
// server over the same session dir (simulating the restart), then:
//
//   - (a) GET the child's session info WITHOUT ever running a turn on it —
//     lineage.parent_id must still be emitted, straight from disk
//     (Session.TaskParentID), with no SessionManager adoption required.
//   - (b) POST a prompt to the ROOT — its own next turn — with NOTHING in
//     this test ever naming the crashed child's id again. The root's
//     OWN next turn must synthesize and deliver the crashed child's
//     lost-to-restart notification (SessionManager.recoverCrashedChildrenLocked,
//     wired into ReportTurnStart's adopt-on-first-sight path via
//     adoptRootLocked), proven by the notification showing up as ambient
//     [tasks:] context in that very turn's own request.
func TestRestartRecoveryEngagesWithoutTouchingCrashedChildDirectly(t *testing.T) {
	dir := t.TempDir()
	rootProv := &scriptedProvider{name: "root", turns: [][]provider.Event{}}
	childProv := newBlockingProvider("child")

	h1 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil, rootProv, childProv)
	rootID := h1.createSession("root/m1")

	resp, data := h1.do("POST", "/session", map[string]string{
		"parent_id": rootID, "agent": engine.AgentGeneralPurpose, "prompt": "go", "model": "child/m1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spawn child status %d: %s", resp.StatusCode, data)
	}
	var child struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &child)

	<-childProv.started // child now genuinely mid-turn, blocked

	// Simulate "kill -9 1": h1 is simply abandoned here, never closed
	// cleanly, never released — exactly as if the process died with the
	// child's own model call still in flight. h2 below is a fully
	// independent server (its own SessionManager, its own provider
	// instances) sharing only the on-disk session dir.
	rootProv2 := &engineContextCaptureProv{name: "root"}
	childProv2 := newBlockingProvider("child")
	h2 := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil, rootProv2, childProv2)

	// (a) Cold GET on the CHILD, with nothing else ever having touched it
	// in this process. No AdoptReloaded, no Send, no prompt against
	// child.ID anywhere in this test.
	resp, data = h2.do("GET", "/session/"+child.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cold GET child status %d: %s", resp.StatusCode, data)
	}
	var coldChild struct {
		Lineage map[string]any `json:"lineage"`
	}
	mustUnmarshal(t, data, &coldChild)
	if coldChild.Lineage == nil {
		t.Fatal("cold child lineage is nil — durable fallback did not fire at all")
	}
	if coldChild.Lineage["parent_id"] != rootID {
		t.Errorf("cold child lineage.parent_id = %v, want %q — emitted straight from disk (TaskParentID), no adoption required", coldChild.Lineage["parent_id"], rootID)
	}

	// (b) The ROOT's own next turn. child.ID is never named again,
	// anywhere below.
	resp, data = h2.do("POST", "/session/"+rootID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "continue"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	resp, data = h2.do("GET", "/session/"+rootID+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait until=idle status %d: %s", resp.StatusCode, data)
	}

	rootProv2.mu.Lock()
	blocks := append([]string(nil), rootProv2.blocks...)
	rootProv2.mu.Unlock()
	if len(blocks) == 0 {
		t.Fatal("root's own next-turn request carried no EngineContext part at all")
	}
	found := false
	for _, b := range blocks {
		if strings.Contains(b, "lost to restart") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("root's own next-turn EngineContext blocks = %v, want one to carry the crashed child's lost-to-restart notification", blocks)
	}

	// Durable proof the CHILD itself was actually recovered, not just
	// that some unrelated text happened to match.
	info, ok := h2.srv.sessMgr.Info(child.ID)
	if !ok {
		t.Fatal("child not tracked by sessMgr after the root's own turn — recoverCrashedChildrenLocked never adopted it")
	}
	if info.Status != engine.StatusFailed || !strings.Contains(info.FailReason, "restart") {
		t.Errorf("child info = %+v, want StatusFailed with a restart-loss fail_reason", info)
	}
}
