package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/majorcontext/harness/engine"
)

// TestListDoesNotReplayNonResidentSessionsRepeatedly is the churn guard.
//
// A control-plane activity probe polls GET /session every ~20s forever. The
// listing renders a non-resident session from its metadata index, but when
// that index cannot answer — a journal whose header predates the workdir
// field, so SessionIndex.Complete is false — it falls back to the
// authoritative engine.LoadSession. That fallback used to throw the loaded
// session away, so every poll re-replayed the same journal cold, forever:
// the `reason=start` context-window churn observed on a box whose finished
// sub-agent sessions were re-loaded on a 20s cadence for the life of the
// process.
//
// The assertion is the specific behavior — at most ONE cold load per
// session across N list calls — not a raw total.
func TestListDoesNotReplayNonResidentSessionsRepeatedly(t *testing.T) {
	dir := t.TempDir()
	// A legacy-shaped journal: a session header with no workdir, so its
	// metadata index is usable but INCOMPLETE, which is exactly the case
	// the listing answers with a full load.
	const legacy = "ses_0123456789abcdef"
	journal := `{"type":"session","id":"` + legacy + `","created_at":"2026-01-02T03:04:05Z"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"hi"}]}}
{"type":"message","message":{"id":"msg_2","role":"assistant","parts":[{"type":"text","text":"yo"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, legacy+".jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	// A second session whose index CAN answer: the listing must render it
	// from metadata alone and never load it at all, not even once.
	indexed := coldSession(t, dir, nil).ID
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	// Count cold loads per session id, through the same seam every handler
	// loads by.
	var mu sync.Mutex
	loads := map[string]int{}
	inner := h.srv.opts.LoadSession
	h.srv.opts.LoadSession = func(id string) (*engine.Session, error) {
		mu.Lock()
		loads[id]++
		mu.Unlock()
		return inner(id)
	}

	const polls = 4
	for i := range polls {
		resp, data := h.do("GET", "/session", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("poll %d: GET /session = %d: %s", i, resp.StatusCode, data)
		}
		var list []sessionJSON
		if err := json.Unmarshal(data, &list); err != nil {
			t.Fatalf("poll %d: decode list: %v (%s)", i, err, data)
		}
		var found *sessionJSON
		for j := range list {
			if list[j].ID == legacy {
				found = &list[j]
			}
		}
		if found == nil {
			t.Fatalf("poll %d: session %s missing from listing %s", i, legacy, data)
		}
		// The entry must stay renderable on every poll, not just the first.
		if found.Model.String() != "test/m1" {
			t.Errorf("poll %d: model = %q, want test/m1", i, found.Model.String())
		}
		if found.Messages != 2 {
			t.Errorf("poll %d: messages = %d, want 2", i, found.Messages)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for id, n := range loads {
		if n > 1 {
			t.Errorf("GET /session cold-loaded %s %d times across %d polls, want at most 1", id, n, polls)
		}
	}
	if n := loads[indexed]; n != 0 {
		t.Errorf("GET /session cold-loaded %s %d times, want 0: its index can answer", indexed, n)
	}
}
