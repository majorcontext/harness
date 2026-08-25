package server

import (
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// spawnSettledChild creates a root, spawns one child through the real
// POST /session parent_id route, and returns both ids once the child's
// own turn has settled (lineage.status "done"). The wait blocks on
// engine.SessionManager.Changed inside waitForLineageStatus — never a
// sleep, never a sampled interval.
func spawnSettledChild(t *testing.T, h *harness) (rootID, childID string) {
	t.Helper()
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
	waitForLineageStatus(t, h, child.ID, "done", 2*time.Second)
	return root.ID, child.ID
}

// TestBuildSessionAnswersFromOneSnapshotAcrossAReap pins the property the
// liveSession refactor exists for: ONE request answers from ONE snapshot
// of the session tree.
//
// GET /session/{id} runs in two halves — Server.lookup resolves the
// session, then Server.buildSession renders it, lineage block included.
// Before this change each half read engine.SessionManager separately
// (lookup via SessionAndInfo, lineageJSONFor via its own Info call), so a
// Reap landing between them described two different nodes in one
// response: the body's own fields came from the tracked node, and the
// lineage block fell through to the durable cold branch, reporting a
// blank status for a child the same response had just rendered from the
// manager's own live object.
//
// The test drives those two production halves directly and reaps between
// them — the one point a concurrent Reap could ever land — then asserts
// the rendered response still describes the node the snapshot captured.
// Reap is the real mutation here, not a stand-in: it is the only
// operation that removes a live node, and the audit behind PR #157 hit it
// for a settled child exactly like this one.
func TestBuildSessionAnswersFromOneSnapshotAcrossAReap(t *testing.T) {
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("the answer is 42")}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)
	rootID, childID := spawnSettledChild(t, h)

	lv, ok := h.srv.lookup(childID)
	if !ok {
		t.Fatalf("lookup(%s) not found", childID)
	}
	if !lv.isManaged {
		t.Fatalf("test setup: child %s is not tracked by SessionManager, so no snapshot property is under test", childID)
	}

	// The whole point: the node disappears between the two halves of one
	// request. Reap removes the settled leaf child (not the root).
	if n := h.srv.sessMgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1 (the settled child)", n)
	}
	if _, tracked := h.srv.sessMgr.Info(childID); tracked {
		t.Fatal("test setup: child still tracked after Reap, so the snapshot is never actually stressed")
	}

	got := h.srv.buildSession(lv)
	if got.ID != childID {
		t.Fatalf("buildSession id = %q, want %q", got.ID, childID)
	}
	if got.Lineage == nil {
		t.Fatal("buildSession lineage = nil, want the captured node's lineage")
	}
	if got.Lineage.Status != string(engine.StatusDone) {
		t.Errorf("buildSession lineage.status = %q, want %q — the response re-read SessionManager instead of reading the snapshot lookup already took", got.Lineage.Status, engine.StatusDone)
	}
	if got.Lineage.Result != "the answer is 42" {
		t.Errorf("buildSession lineage.result = %q, want %q (same captured node)", got.Lineage.Result, "the answer is 42")
	}
	if got.Lineage.ParentID != rootID {
		t.Errorf("buildSession lineage.parent_id = %q, want %q", got.Lineage.ParentID, rootID)
	}
	if got.Status != "idle" {
		t.Errorf("buildSession status = %q, want \"idle\" (the captured node is settled)", got.Status)
	}
}

// TestResolveLivePairsManagedSessionWithItsOwnNode hammers the invariant
// engine.SessionManager.SessionAndInfo exists to hold, through the
// server's one resolver: the manager half of a liveSession is a PAIR, and
// a snapshot either has both parts or neither.
//
// Two separate SessionManager reads (Session, then Info) can straddle a
// Reap. The first returns a live *engine.Session, the second reports the
// id as untracked — the snapshot then holds a session it claims nothing
// tracks, and status()/lineageJSONFor answer from the wrong branch for a
// child the manager was driving a moment earlier. SessionAndInfo takes
// both under one m.mu hold, so the pair cannot split; this test proves the
// resolver keeps using it.
//
// The mutator loop reaps the settled child and re-adopts the same session
// object, over and over, while a second goroutine resolves that id as
// fast as it can. No sleeps and no timers: the resolver stops when the
// mutator closes done, and a genuine hang is the test binary's own
// timeout to catch.
func TestResolveLivePairsManagedSessionWithItsOwnNode(t *testing.T) {
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{asstTurn("the answer is 42")}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"}, childProv)
	_, childID := spawnSettledChild(t, h)

	childSess, ok := h.srv.sessMgr.Session(childID)
	if !ok {
		t.Fatalf("child %s not tracked", childID)
	}

	const mutations = 3000
	done := make(chan struct{})
	reaps := make(chan int, 1)
	go func() {
		defer close(done)
		reaped := 0
		for i := 0; i < mutations; i++ {
			if h.srv.sessMgr.Reap() == 0 {
				continue
			}
			reaped++
			if err := h.srv.sessMgr.AdoptReloaded(childSess); err != nil {
				// Re-adoption is the loop's own precondition for the
				// next Reap. Report and stop rather than spin against a
				// tree that no longer holds the child.
				t.Errorf("AdoptReloaded(%s) on iteration %d: %v", childID, i, err)
				break
			}
		}
		reaps <- reaped
	}()

	resolves, split := 0, 0
	for {
		select {
		case <-done:
			reaped := <-reaps
			if reaped == 0 {
				t.Fatal("test setup: the mutator never reaped the child, so no read ever raced a removal")
			}
			if split > 0 {
				t.Errorf("resolveLive returned %d split manager halves out of %d resolves (a session with no node, or a node with no session) — the resolver is reading Session and Info separately", split, resolves)
			}
			if resolves == 0 {
				t.Fatal("test setup: the resolver goroutine never ran")
			}
			return
		default:
		}
		lv := h.srv.resolveLive(childID)
		resolves++
		if (lv.managed != nil) != lv.isManaged {
			split++
		}
	}
}

// TestResolveLivePrefersResidentOverManagerObject pins the preference
// order the four merged call sites all depended on: a resident session
// wins outright, and its own running flag — never SessionManager's node
// status — answers for it.
//
// freeRunSlotAndEmitIdle clears the resident running flag and wakes
// waiters BEFORE ReportTurnEnd flips the manager node off StatusRunning,
// so a resolver that preferred the manager (or merged the two) would
// report a turn this server already finished as still busy. That exact
// regression reached review once on waitSnapshot; this states the rule
// once, for every caller of the shared snapshot.
func TestResolveLivePrefersResidentOverManagerObject(t *testing.T) {
	dir := t.TempDir()
	h := multiProviderHarnessInDir(t, dir, message.ModelRef{Provider: "root", Model: "m1"}, nil,
		&scriptedProvider{name: "root"})

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create root status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	// A created root is BOTH resident and managed, so the preference is
	// actually exercised rather than trivially satisfied.
	lv := h.srv.resolveLive(root.ID)
	if lv.resident == nil || !lv.isManaged {
		t.Fatalf("test setup: root resident=%v managed=%v, want both", lv.resident != nil, lv.isManaged)
	}
	if lv.session() != lv.resident {
		t.Errorf("session() = %p, want the resident object %p", lv.session(), lv.resident)
	}

	// Force the two halves to disagree: the manager node reads running,
	// residency does not. Residency must still win.
	rootSess, ok := h.srv.sessMgr.Session(root.ID)
	if !ok {
		t.Fatalf("root %s not tracked", root.ID)
	}
	h.srv.sessMgr.ReportTurnStart(rootSess)
	lv = h.srv.resolveLive(root.ID)
	if info, ok := h.srv.sessMgr.Info(root.ID); !ok || info.Status != engine.StatusRunning {
		t.Fatalf("test setup: manager status = %v, want running", info.Status)
	}
	if lv.running {
		t.Fatal("test setup: the resident entry reads running, so the two halves do not disagree")
	}
	if got := lv.status(); got != "idle" {
		t.Errorf("status() = %q, want \"idle\" — a resident session's own running flag is authoritative for itself", got)
	}
}
