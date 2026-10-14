package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// browserTurn is the answer of one prompt that a tab ran.
type browserTurn struct {
	StopReason string `json:"stopReason"`
	Text       string `json:"text"`
}

// browserBoot is the answer of a tab that tried to start the harness.
type browserBoot struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

// browserStore is the persistent store of the origin as a tab reads it: the log
// lines and the blob sizes of each session.
type browserStore struct {
	Sessions map[string]struct {
		Lines []string       `json:"lines"`
		Bad   bool           `json:"bad"`
		Blobs map[string]int `json:"blobs"`
	} `json:"sessions"`
}

// log returns the lines of a session, or fails when the store has no such session.
func (s browserStore) log(t *testing.T, id string) []string {
	t.Helper()
	got, ok := s.Sessions[id]
	if !ok {
		t.Fatalf("the store has no session %q: %d sessions", id, len(s.Sessions))
	}
	if got.Bad {
		t.Errorf("session %q has a torn or unreadable log line", id)
	}
	return got.Lines
}

func (s browserStore) holds(t *testing.T, id, text string) {
	t.Helper()
	if !strings.Contains(strings.Join(s.log(t, id), "\n"), text) {
		t.Errorf("the log of session %q lacks %q", id, text)
	}
}

func rowData(t *testing.T, res browserResult, into any) {
	t.Helper()
	if err := json.Unmarshal(res.Raw, into); err != nil {
		t.Fatalf("result of the row: %v\n%s", err, res.Raw)
	}
}

func (k *browserKit) answers(texts ...string) []harnesstest.Step {
	steps := make([]harnesstest.Step, len(texts))
	for i, s := range texts {
		steps[i] = k.text(s)
	}
	return steps
}

// storeRows adds the rows of the persistent store: a reload, a second tab, a
// hand-off, a blob, and a Worker kill, then the file tools in the work directory.
func (k *browserKit) storeRows() {
	k.reloadRows()
	k.tabRows()
	k.durabilityRows()
	k.fileToolRows()
}

func (k *browserKit) reloadRows() {
	t, add, noFailure := k.t, k.add, k.noFailure
	f := newBrowserFront(t, nil, k.answers("first answer", "second answer")...)
	add("a reload keeps the records of a session", map[string]any{"kind": "reload", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
		noFailure(t, res)
		var r struct {
			SessionID     string
			First, Second browserTurn
			Stored, Final browserStore
			Boot          browserBoot
			Resumed       struct{ Error json.RawMessage }
		}
		rowData(t, res, &r)
		if r.First.Text != "first answer" || r.Second.Text != "second answer" || !r.Boot.OK {
			t.Fatalf("turns %+v and %+v, boot %+v", r.First, r.Second, r.Boot)
		}
		if len(r.Resumed.Error) > 0 {
			t.Errorf("session/resume after the reload: %s", r.Resumed.Error)
		}
		r.Stored.holds(t, r.SessionID, "first answer")
		r.Stored.holds(t, r.SessionID, `"one"`)
		r.Final.holds(t, r.SessionID, "second answer")
		if got := r.Final.log(t, r.SessionID); !slices.Equal(got[:len(r.Stored.log(t, r.SessionID))], r.Stored.log(t, r.SessionID)) {
			t.Error("the records of the first turn changed after the resume")
		}
		reqs := f.fake.Requests()
		if len(reqs) != 2 || !strings.Contains(requestText(reqs[1]), "first answer") {
			t.Errorf("the model request after the resume lacks the history: %d requests", len(reqs))
		}
	})
}

func (k *browserKit) tabRows() {
	k.heldStoreRow()
	k.handoffRow()
	k.simultaneousTabsRow()
}

func (k *browserKit) heldStoreRow() {
	t, add, noFailure := k.t, k.add, k.noFailure
	{
		f := newBrowserFront(t, nil, k.answers("answer one", "answer two")...)
		add("a second tab on a held store gets store_in_use and the first tab loses no record", map[string]any{"kind": "twoTabs", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			var r struct {
				SessionID    string
				First, Again browserTurn
				Refused      browserBoot
				Final        browserStore
			}
			rowData(t, res, &r)
			if r.Refused.OK || r.Refused.Code != "store_in_use" {
				t.Errorf("the second tab = %+v, want store_in_use", r.Refused)
			}
			if r.First.Text != "answer one" || r.Again.Text != "answer two" {
				t.Errorf("turns %+v and %+v", r.First, r.Again)
			}
			r.Final.holds(t, r.SessionID, "answer one")
			r.Final.holds(t, r.SessionID, "answer two")
			if n := len(r.Final.Sessions); n != 1 {
				t.Errorf("the store holds %d sessions, want 1", n)
			}
		})
	}
}

func (k *browserKit) handoffRow() {
	t, add, noFailure := k.t, k.add, k.noFailure
	{
		f := newBrowserFront(t, nil, k.answers("answer one", "answer two")...)
		add("a second tab boots after the first closes and sees every record", map[string]any{"kind": "handoff", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			var r struct {
				SessionID, SecondID string
				First, Second       browserTurn
				Seen, Final         browserStore
				Boot                browserBoot
			}
			rowData(t, res, &r)
			if !r.Boot.OK || r.First.Text != "answer one" || r.Second.Text != "answer two" {
				t.Fatalf("boot %+v, turns %+v and %+v", r.Boot, r.First, r.Second)
			}
			r.Seen.holds(t, r.SessionID, "answer one")
			if !slices.Equal(r.Final.log(t, r.SessionID), r.Seen.log(t, r.SessionID)) {
				t.Error("the first session changed after the hand-off")
			}
			r.Final.holds(t, r.SecondID, "answer two")
		})
	}
}

func (k *browserKit) simultaneousTabsRow() {
	t, add, noFailure := k.t, k.add, k.noFailure
	{
		f := newBrowserFront(t, nil, k.unused("answer"))
		add("two tabs that start at once: one creates in the store and the other runs in memory", map[string]any{"kind": "differentSessions", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			var r struct {
				Tabs []struct {
					Winner bool
					Code   string
					Turn   browserTurn
				}
				Final browserStore
			}
			rowData(t, res, &r)
			if len(r.Tabs) != 2 || r.Tabs[0].Winner == r.Tabs[1].Winner {
				t.Fatalf("tabs = %+v, want exactly one winner", r.Tabs)
			}
			var winner int
			for i, tab := range r.Tabs {
				if tab.Turn.StopReason != "end_turn" {
					t.Errorf("tab %d: stopReason %q", i+1, tab.Turn.StopReason)
				}
				if tab.Winner {
					winner = i + 1
				} else if tab.Code != "store_in_use" {
					t.Errorf("tab %d failed with code %q, want store_in_use", i+1, tab.Code)
				}
			}
			if len(r.Final.Sessions) != 1 {
				t.Fatalf("the store holds %d sessions, want 1", len(r.Final.Sessions))
			}
			for id := range r.Final.Sessions {
				r.Final.holds(t, id, "from tab "+string(rune('0'+winner)))
				r.Final.holds(t, id, "answer")
				if strings.Contains(strings.Join(r.Final.log(t, id), "\n"), "from tab "+string(rune('0'+3-winner))) {
					t.Error("the store holds a record of the tab that has the memory store")
				}
			}
		})
	}
}

func (k *browserKit) durabilityRows() {
	t, add, noFailure := k.t, k.add, k.noFailure
	{
		call := harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "js_big", Input: map[string]any{}}}}}
		read := harnesstest.Step{Name: "read", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c2", Name: "read_tool_result", Input: map[string]any{"handle": "trh_1", "search": "line 1999"}}}}}
		f := newBrowserFront(t, nil, call, read, k.text("done"))
		add("a retained tool result is a blob that reads back and survives a reload", map[string]any{"kind": "blob", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			var r struct {
				SessionID string
				Turn      browserTurn
				Stored    browserStore
			}
			rowData(t, res, &r)
			if r.Turn.StopReason != "end_turn" {
				t.Fatalf("turn = %+v", r.Turn)
			}
			blobs := r.Stored.Sessions[r.SessionID].Blobs
			if len(blobs) != 1 {
				t.Fatalf("blobs = %v, want one blob and no temporary file", blobs)
			}
			for key, size := range blobs {
				t.Logf("blob %q holds %d bytes", key, size)
				if size != 18889 {
					t.Errorf("blob %q holds %d bytes, want 18889", key, size)
				}
			}
			reqs := f.fake.Requests()
			if len(reqs) != 3 {
				t.Fatalf("model requests = %d, want 3", len(reqs))
			}
			if requestText(reqs[1]) == "" || strings.Contains(requestText(reqs[1]), "line 1999") {
				t.Error("the preview of the retained result holds its tail")
			}
			if !strings.Contains(requestText(reqs[2]), "line 1999") {
				t.Error("read_tool_result does not read the tail from the blob")
			}
		})
	}
	{
		f := newBrowserFront(t, nil, k.text("answer before the kill"))
		add("a Worker kill after an acknowledged turn loses no record", map[string]any{"kind": "kill", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
			noFailure(t, res)
			var r struct {
				SessionID string
				Turn      browserTurn
				Stored    browserStore
			}
			rowData(t, res, &r)
			if r.Turn.StopReason != "end_turn" {
				t.Fatalf("turn = %+v", r.Turn)
			}
			r.Stored.holds(t, r.SessionID, "answer before the kill")
			t.Logf("log lines after the kill: %d", len(r.Stored.log(t, r.SessionID)))
		})
	}
}

func (k *browserKit) fileToolRows() {
	t, add, noFailure := k.t, k.add, k.noFailure
	steps := toolChain(
		ftWrite("/work/note.txt", "alpha\nbeta\n"),
		ftRead(ftArgs("path", "/work/note.txt")),
		ftEdit("note.txt", "beta", "gamma"),
		ftRead(ftArgs("path", "note.txt")),
		ftWrite("/work/AGENTS.md", "Marker rule: prefer tabs.\n"),
		ftWrite("/work/sub/deep.txt", "needle in sub\n"),
		ftTool("ls", ftArgs("path", "/work")),
		ftTool("glob", ftArgs("pattern", "**/*.txt", "path", "/work")),
		ftTool("grep", ftArgs("pattern", "needle", "path", "/work")),
	)
	again := harnesstest.Step{Name: "again", Match: func(r harnesstest.Request) bool { return strings.Contains(r.System, "Marker rule") }, Reply: harnesstest.Reply{Text: "second"}}
	f := newBrowserFront(t, nil, append(steps, again)...)
	add("file tools write, read, edit, list, glob, and grep in the work directory", map[string]any{"kind": "files", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
		noFailure(t, res)
		if res.StopReason != "end_turn" {
			t.Fatalf("stopReason = %q, error %+v, console %v", res.StopReason, res.Error, res.Console)
		}
		var r struct{ AgainStop string }
		rowData(t, res, &r)
		if r.AgainStop != "end_turn" {
			t.Errorf("the second session ended %q: AGENTS.md of the work directory did not reach its system prompt", r.AgainStop)
		}
		reqs := f.fake.Requests()
		if len(reqs) != 11 {
			t.Fatalf("model requests = %d, want 11", len(reqs))
		}
		for i, want := range map[int]string{2: "beta", 4: "gamma", 7: "deep.txt", 8: "sub/deep.txt", 9: "needle in sub"} {
			if got := requestText(reqs[i]); !strings.Contains(got, want) {
				t.Errorf("request %d lacks %q: %s", i, want, got)
			}
		}
	})
}
