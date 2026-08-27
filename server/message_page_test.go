package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// pageResponse mirrors messagePageJSON for a test that decodes it as a
// client would, without reaching into the server's own type for anything
// but the field names.
type pageResponse struct {
	Messages []message.Message `json:"messages"`
	FirstSeq int               `json:"first_seq"`
	LastSeq  int               `json:"last_seq"`
	Total    int               `json:"total"`
	HasMore  bool              `json:"has_more"`
}

// coldMessages writes a session with n turns into dir through the engine's
// own path, so the server sees it exactly as it sees any session it did not
// create: a journal on disk, nothing resident.
func coldMessages(t *testing.T, dir string, n int) *engine.Session {
	t.Helper()
	turns := make([][]provider.Event, 0, n)
	for i := 0; i < n; i++ {
		turns = append(turns, asstTurn(fmt.Sprintf("reply %d", i)))
	}
	prov := &scriptedProvider{name: "test", turns: turns}
	sess := engine.NewSession(engine.Config{
		Providers:  provider.Registry{prov.name: prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
		WorkDir:    dir,
	})
	for i := 0; i < n; i++ {
		if _, err := sess.Prompt(context.Background(), fmt.Sprintf("ask %d", i)); err != nil {
			t.Fatalf("Prompt %d: %v", i, err)
		}
	}
	if err := sess.PersistErr(); err != nil {
		t.Fatalf("PersistErr: %v", err)
	}
	return sess
}

func getPage(t *testing.T, h *harness, id, query string) pageResponse {
	t.Helper()
	resp, data := h.do("GET", "/session/"+id+"/message"+query, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message%s = %d: %s", query, resp.StatusCode, data)
	}
	var page pageResponse
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatalf("decode page: %v (%s)", err, data)
	}
	return page
}

// TestMessagesUnparameterizedIsUnchanged pins the compatibility promise: a
// caller that asks for no page still gets the bare array of the WHOLE
// history it has always got, not an envelope.
func TestMessagesUnparameterizedIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	sess := coldMessages(t, dir, 3)
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/session/"+sess.ID+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message = %d: %s", resp.StatusCode, data)
	}
	var msgs []message.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatalf("unparameterized response must stay a bare array: %v (%s)", err, data)
	}
	if len(msgs) != 6 {
		t.Errorf("got %d messages, want 6", len(msgs))
	}
}

// TestMessagePageServesBoundedTail is the endpoint's reason to exist: a
// console asks for the newest K and gets K, plus where they sit.
func TestMessagePageServesBoundedTail(t *testing.T) {
	dir := t.TempDir()
	sess := coldMessages(t, dir, 5) // 10 messages
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	page := getPage(t, h, sess.ID, "?limit=4")
	if len(page.Messages) != 4 {
		t.Fatalf("got %d messages, want 4", len(page.Messages))
	}
	if page.Total != 10 {
		t.Errorf("total = %d, want 10", page.Total)
	}
	if page.FirstSeq != 7 || page.LastSeq != 10 {
		t.Errorf("seqs = [%d,%d], want [7,10]", page.FirstSeq, page.LastSeq)
	}
	if !page.HasMore {
		t.Error("has_more = false, want true")
	}
}

// TestMessagePageScrollsBackToTheStart drives the console's own loop: fetch
// the tail, then page older with before_seq, until has_more is false. The
// pages must reassemble into the full transcript exactly once.
func TestMessagePageScrollsBackToTheStart(t *testing.T) {
	dir := t.TempDir()
	sess := coldMessages(t, dir, 4) // 8 messages
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/session/"+sess.ID+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message = %d: %s", resp.StatusCode, data)
	}
	var whole []message.Message
	if err := json.Unmarshal(data, &whole); err != nil {
		t.Fatal(err)
	}

	var got []message.Message
	query := "?limit=3"
	for {
		page := getPage(t, h, sess.ID, query)
		got = append(append([]message.Message{}, page.Messages...), got...)
		if !page.HasMore {
			break
		}
		query = fmt.Sprintf("?limit=3&before_seq=%d", page.FirstSeq)
	}
	if len(got) != len(whole) {
		t.Fatalf("paged walk returned %d messages, want %d", len(got), len(whole))
	}
	for i := range got {
		if got[i].ID != whole[i].ID {
			t.Fatalf("paged walk differs at %d: %q, want %q", i, got[i].ID, whole[i].ID)
		}
	}
}

// TestMessagePageServesRunningSession: a page must be answerable while the
// session is mid-turn. It reads the durable records, so it neither waits
// for the turn nor reports the turn's unfinished state.
func TestMessagePageServesRunningSession(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	<-prov.started
	defer prov.releaseAll()

	page := getPage(t, h, id, "?limit=10")
	if page.Total != 1 || len(page.Messages) != 1 {
		t.Fatalf("page = %+v, want the one durable user message", page)
	}
	if page.Messages[0].Role != message.RoleUser {
		t.Errorf("role = %q, want user", page.Messages[0].Role)
	}
}

// TestMessagePageOnNeverPersistedSession: a session created through the API
// and never prompted has no messages at all. The page endpoint answers an
// empty page, never a 404 or a 500.
func TestMessagePageOnNeverPersistedSession(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("")

	page := getPage(t, h, id, "?limit=5")
	if len(page.Messages) != 0 || page.Total != 0 || page.HasMore {
		t.Errorf("page = %+v, want an empty page", page)
	}
}

// TestMessagePageRejectsBadParameters: a malformed page request is a client
// error, not a silently-defaulted one — a caller that sends limit=abc has a
// bug and must be told.
func TestMessagePageRejectsBadParameters(t *testing.T) {
	dir := t.TempDir()
	sess := coldMessages(t, dir, 1)
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	for _, query := range []string{"?limit=abc", "?limit=-1", "?before_seq=nope", "?before_seq=-3", "?limit=", "?before_seq="} {
		resp, data := h.do("GET", "/session/"+sess.ID+"/message"+query, nil)
		if resp.StatusCode != 400 {
			t.Errorf("GET message%s = %d, want 400: %s", query, resp.StatusCode, data)
		}
	}
}

// TestMessagePageUnknownSessionIsNotFound: an id with no journal and no
// live session is a 404, exactly as the unparameterized read reports it.
func TestMessagePageUnknownSessionIsNotFound(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, _ := h.do("GET", "/session/ses_0123456789abcdef/message?limit=5", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("GET unknown session page = %d, want 404", resp.StatusCode)
	}
}

// TestMessagePageBeforeSeqOne: paging older than the oldest message is an
// empty page with has_more false — the loop terminator a client relies on.
func TestMessagePageBeforeSeqOne(t *testing.T) {
	dir := t.TempDir()
	sess := coldMessages(t, dir, 2)
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	page := getPage(t, h, sess.ID, "?before_seq=1&limit=5")
	if len(page.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(page.Messages))
	}
	if page.HasMore {
		t.Error("has_more = true, want false")
	}
	if page.Total != 4 {
		t.Errorf("total = %d, want 4", page.Total)
	}
}

// TestMessagePageDistinguishesMissingFromUnreadable: a session whose
// journal cannot be read is not "no such session". Reporting 404 for it
// sends an operator looking for a session id that is on disk in front of
// them.
func TestMessagePageDistinguishesMissingFromUnreadable(t *testing.T) {
	dir := t.TempDir()
	id := "ses_0123456789abcdef"
	// A journal whose SECOND record is corrupt: scanLog's tolerance covers
	// only a corrupt final line, so this file exists and cannot be folded.
	journal := `{"type":"session","id":"ses_0123456789abcdef","created_at":"2026-01-02T03:04:05Z","workdir":"/w"}
{"type":"message","message":{"id":"msg_1","ro
{"type":"message","message":{"id":"msg_2","role":"user","parts":[{"type":"text","text":"hi"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/session/"+id+"/message?limit=5", nil)
	if resp.StatusCode != 500 {
		t.Errorf("unreadable journal = %d: %s; want 500", resp.StatusCode, data)
	}

	resp, data = h.do("GET", "/session/ses_00000000000000000000000000/message?limit=5", nil)
	if resp.StatusCode != 404 {
		t.Errorf("absent journal = %d: %s; want 404", resp.StatusCode, data)
	}
}

// TestMessagePageRejectsRepeatedParameters: "?limit=2&limit=nonsense" names
// two intentions. Answering the first hides a client bug.
func TestMessagePageRejectsRepeatedParameters(t *testing.T) {
	dir := t.TempDir()
	sess := coldMessages(t, dir, 1)
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	for _, query := range []string{"?limit=2&limit=3", "?before_seq=1&before_seq=2", "?limit=2&limit=nonsense"} {
		resp, data := h.do("GET", "/session/"+sess.ID+"/message"+query, nil)
		if resp.StatusCode != 400 {
			t.Errorf("GET message%s = %d, want 400: %s", query, resp.StatusCode, data)
		}
	}
}

// TestMessagePageKeepsTheDurableContractWhenAJournalIsUnreadable: a live
// session's resident history can carry messages the log does not — a repair
// applied at load, or recovery's memory-only closer. Paging it would give
// those messages sequence numbers, and a client that paged again after the
// journal became readable would see its pages renumbered. An unreadable
// journal is therefore a 500, even for a session this process holds live.
func TestMessagePageKeepsTheDurableContractWhenAJournalIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("hi")}})
	id := h.createSession("")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	// Corrupt a NON-final record, which no reader tolerates, while the
	// session stays resident.
	path := filepath.Join(dir, id+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) < 4 {
		t.Fatalf("test setup: journal has %d lines", len(lines))
	}
	lines[2] = `{"type":"message","message":{"id":"broken`
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, data = h.do("GET", "/session/"+id+"/message?limit=5", nil)
	if resp.StatusCode != 500 {
		t.Errorf("unreadable journal for a LIVE session = %d: %s; want 500, never resident history under durable seqs", resp.StatusCode, data)
	}
}

// TestMessagePageFallbackNumbersTheDurableSequence: the resident fallback
// is the one place a page is numbered from memory rather than from records,
// and it must number the SAME sequence a journal page would. This drives a
// live session whose journal is gone — the only case the fallback serves —
// and checks the page carries the durable seqs, not a memory-only count.
func TestMessagePageFallbackNumbersTheDurableSequence(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one")}})
	id := h.createSession("")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	// A page from the journal, then the same page with the journal gone.
	fromJournal := getPage(t, h, id, "?limit=10")
	if err := os.Remove(filepath.Join(dir, id+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, id+".index.json")); err != nil {
		t.Fatal(err)
	}
	fromMemory := getPage(t, h, id, "?limit=10")

	if fromMemory.Total != fromJournal.Total {
		t.Errorf("total = %d from memory, %d from the journal", fromMemory.Total, fromJournal.Total)
	}
	if fromMemory.FirstSeq != fromJournal.FirstSeq || fromMemory.LastSeq != fromJournal.LastSeq {
		t.Errorf("seqs = [%d,%d] from memory, [%d,%d] from the journal",
			fromMemory.FirstSeq, fromMemory.LastSeq, fromJournal.FirstSeq, fromJournal.LastSeq)
	}
	if len(fromMemory.Messages) != len(fromJournal.Messages) {
		t.Fatalf("%d messages from memory, %d from the journal", len(fromMemory.Messages), len(fromJournal.Messages))
	}
	for i := range fromMemory.Messages {
		if fromMemory.Messages[i].ID != fromJournal.Messages[i].ID {
			t.Errorf("message %d: %q from memory, %q from the journal", i, fromMemory.Messages[i].ID, fromJournal.Messages[i].ID)
		}
	}
}

// TestDurableOnlyDropsDerivedRepairMessages pins the filter the fallback
// leans on. message.ResolveOrphanToolCalls derives a tool result for a call
// whose result never reached the log; that message has no record, so it has
// no byte offset and no sequence number. A page must never give it one.
func TestDurableOnlyDropsDerivedRepairMessages(t *testing.T) {
	history := []message.Message{
		{ID: "msg_u1", Role: message.RoleUser},
		{ID: "msg_a1", Role: message.RoleAssistant},
		{ID: message.SyntheticOrphanIDPrefix + "1-tc1", Role: message.RoleTool},
		{ID: "msg_u2", Role: message.RoleUser},
	}
	got := durableOnly(history)
	want := []string{"msg_u1", "msg_a1", "msg_u2"}
	if len(got) != len(want) {
		t.Fatalf("durableOnly returned %d messages, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ID != want[i] {
			t.Errorf("message %d = %q, want %q", i, got[i].ID, want[i])
		}
	}
}

// TestMessagePageRejectsAnOversizedLimit: the published schema names a
// maximum for `limit`, and a generated client or a gateway enforces it.
// Answering a larger request with a smaller page would make the server
// disagree with its own spec, so the boundary rejects rather than clamps.
// The engine API still clamps, for a caller with no schema to honor.
func TestMessagePageRejectsAnOversizedLimit(t *testing.T) {
	dir := t.TempDir()
	sess := coldMessages(t, dir, 2)
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", fmt.Sprintf("/session/%s/message?limit=%d", sess.ID, engine.MaxMessagePageLimit+1), nil)
	if resp.StatusCode != 400 {
		t.Errorf("limit above the maximum = %d: %s; want 400", resp.StatusCode, data)
	}
	// The maximum itself is accepted.
	resp, data = h.do("GET", fmt.Sprintf("/session/%s/message?limit=%d", sess.ID, engine.MaxMessagePageLimit), nil)
	if resp.StatusCode != 200 {
		t.Errorf("limit at the maximum = %d: %s; want 200", resp.StatusCode, data)
	}
}

// TestMessagePageWindowIsSharedWithTheJournalPath: the resident fallback
// and the journal path must paginate identically. Both call
// engine.MessagePageWindow, and this pins the observable half — the same
// request against the same session yields the same window either way.
func TestMessagePageWindowIsSharedWithTheJournalPath(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("a"), asstTurn("b"), asstTurn("c")}})
	id := h.createSession("")
	for i := 0; i < 3; i++ {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": fmt.Sprintf("go %d", i)}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
		}
		h.waitIdle(id)
	}

	queries := []string{"?limit=2", "?limit=2&before_seq=5", "?limit=100", "?before_seq=1&limit=2"}
	fromJournal := make([]pageResponse, 0, len(queries))
	for _, q := range queries {
		fromJournal = append(fromJournal, getPage(t, h, id, q))
	}
	if err := os.Remove(filepath.Join(dir, id+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, id+".index.json")); err != nil {
		t.Fatal(err)
	}
	for i, q := range queries {
		got := getPage(t, h, id, q)
		want := fromJournal[i]
		if got.FirstSeq != want.FirstSeq || got.LastSeq != want.LastSeq || got.Total != want.Total || got.HasMore != want.HasMore {
			t.Errorf("%s: memory page [%d,%d] total=%d more=%v, journal page [%d,%d] total=%d more=%v",
				q, got.FirstSeq, got.LastSeq, got.Total, got.HasMore, want.FirstSeq, want.LastSeq, want.Total, want.HasMore)
		}
	}
}
