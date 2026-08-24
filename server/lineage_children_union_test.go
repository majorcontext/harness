package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestLineageChildrenSurvivesReapViaDurableUnion is the regression test for
// BUG C: lineage.children on the wire came ONLY from
// engine.SessionManager.Info's live in-memory sessionNode.children —
// Reap() explicitly drops a settled leaf from its parent's Children list
// once reaped (see Reap's own doc comment: "Removing a leaf also drops its
// id from its parent's Children list"), so a parent whose only child had
// already settled and been reaped reported "children":[] even though
// Session.SpawnedChildIDs() — the durable, append-only, NEVER-shrinking
// record of every child a session ever spawned, persisted unconditionally
// at spawn time — still listed it. A caller had no way to see a session's
// full spawn history once any part of it aged out of the live tree.
//
// The fix (childIDsUnion, server/handlers.go's lineageJSONFor): merge the
// live tree's Children with the parent's own already-loaded
// SpawnedChildIDs(), durable entries first (spawn order — see
// childIDsUnion's own doc comment), so the wire list is complete (every
// child ever spawned, live or long since reaped) rather than whichever
// half of the bookkeeping happens to still be resident.
//
// Driven entirely inside a synctest bubble (no real listener, no sleeps):
// spawn a child via the `task` tool, let it settle, Reap it (dropping it
// from root's live Children), then GET /session/{root} and prove
// lineage.children still names it.
func TestLineageChildrenSurvivesReapViaDurableUnion(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		rootProv := &scriptedProvider{name: "root", turns: [][]provider.Event{
			toolCallTurn("tc1", "task", `{"agent":"general-purpose","prompt":"find the answer","model":"child/m1"}`),
			asstTurn("spawned it"),
			asstTurn("noted"), // absorbs the completion-notification resume
		}}
		childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{
			asstTurn("the answer is 42"),
		}}
		reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}

		var srv *Server
		opts := Options{
			SessionDir: dir,
			RunToken:   "secret-run-token",
			Version:    "9.9.9",
			NewSession: func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
				return engine.NewSession(engine.Config{
					Providers:      reg,
					Model:          m,
					WorkDir:        workDir,
					ParentSession:  parentSession,
					SessionDir:     dir,
					OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
					SessionManager: srv.sessMgr,
				}), nil
			},
			LoadSession: func(id string) (*engine.Session, error) {
				return engine.LoadSession(engine.Config{
					Providers:      reg,
					SessionDir:     dir,
					OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
					SessionManager: srv.sessMgr,
				}, id)
			},
		}
		var err error
		srv, err = New(opts)
		if err != nil {
			t.Fatal(err)
		}

		rootID := createSessionDirect(t, srv, "root/m1")

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/session/"+rootID+"/prompt_async",
			strings.NewReader(`{"parts":[{"type":"text","text":"go"}]}`))
		req.SetPathValue("id", rootID)
		srv.handlePrompt(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("prompt_async status %d: %s", rec.Code, rec.Body)
		}
		synctest.Wait()

		info, ok := srv.sessMgr.Info(rootID)
		if !ok || len(info.Children) != 1 {
			t.Fatalf("root lineage children = %v, want exactly 1 spawned child", info.Children)
		}
		childID := info.Children[0]

		if n := srv.sessMgr.Reap(); n != 1 {
			t.Fatalf("Reap() = %d, want 1 (the settled child)", n)
		}
		// Confirm Reap really did drop it from the LIVE tree — otherwise
		// this test would not actually be exercising the durable fallback
		// childIDsUnion exists for.
		if info, ok := srv.sessMgr.Info(rootID); !ok || len(info.Children) != 0 {
			t.Fatalf("root live Children after Reap = %v, want empty (test setup invalid)", info.Children)
		}

		getRec := httptest.NewRecorder()
		getReq := httptest.NewRequest("GET", "/session/"+rootID, nil)
		getReq.SetPathValue("id", rootID)
		srv.handleGet(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("GET /session/%s status %d: %s", rootID, getRec.Code, getRec.Body)
		}
		var got struct {
			Lineage struct {
				Children []string `json:"children"`
			} `json:"lineage"`
		}
		if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, getRec.Body)
		}
		if len(got.Lineage.Children) != 1 || got.Lineage.Children[0] != childID {
			t.Errorf("lineage.children after Reap = %v, want [%q] (from durable SpawnedChildIDs, not the emptied live tree)", got.Lineage.Children, childID)
		}
	})
}

// TestChildIDsUnionPreservesSpawnOrder pins childIDsUnion's ordering
// contract directly. The live tree's child list is always a spawn-order
// subsequence of the durable SpawnedChildIDs list (adoptLocked appends at
// spawn; Reap's filter keeps survivor order), so the union must range
// durable first. A live-first merge reorders siblings the moment an elder
// child settles and is Reaped while a younger one still runs: live=[B],
// durable=[A,B] came out [B, A] instead of [A, B].
func TestChildIDsUnionPreservesSpawnOrder(t *testing.T) {
	cases := []struct {
		name          string
		live, durable []string
		want          []string
	}{
		{"elder reaped, younger live", []string{"B"}, []string{"A", "B"}, []string{"A", "B"}},
		{"all reaped", nil, []string{"A", "B"}, []string{"A", "B"}},
		{"all live, legacy no durable", []string{"A", "B"}, nil, []string{"A", "B"}},
		{"no children", nil, nil, []string{}},
		{"legacy live-only straggler", []string{"A", "C"}, []string{"A", "B"}, []string{"A", "B", "C"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := childIDsUnion(tc.live, tc.durable)
			if got == nil {
				t.Fatal("childIDsUnion returned nil, want a non-nil slice (wire contract: \"children\":[] never null)")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("childIDsUnion(%v, %v) = %v, want %v", tc.live, tc.durable, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("childIDsUnion(%v, %v) = %v, want %v", tc.live, tc.durable, got, tc.want)
				}
			}
		})
	}
}
