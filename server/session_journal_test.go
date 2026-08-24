package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func decodeJournal(t *testing.T, data []byte) JournalResponse {
	t.Helper()
	var resp JournalResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode journal response: %v; body=%s", err, data)
	}
	return resp
}

// TestHandleJournal_UnknownSession404 proves an id this process has never
// heard of — not resident, not on disk — is a plain 404, exactly like
// GET /session/{id} (handleGet).
func TestHandleJournal_UnknownSession404(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, _ := h.do("GET", "/session/ses_0000000000000000000000000000000000000000000000/journal", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleJournal_CreatedNotPrompted_HasHeaderRecordsOnly proves an
// ordinary POST /session already answers with 2 durable records (session
// header, model) even before any prompt: handleCreate eagerly calls
// sess.Persist so a never-prompted session still has durable state (see its
// own doc comment, handlers.go) — the journal must reflect that immediately,
// not just after the first turn.
func TestHandleJournal_CreatedNotPrompted_HasHeaderRecordsOnly(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("GET", "/session/"+id+"/journal", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	got := decodeJournal(t, data)
	if got.SessionID != id {
		t.Errorf("session_id = %q, want %q", got.SessionID, id)
	}
	if len(got.Records) != 2 || got.Records[0].Type != "session" || got.Records[1].Type != "model" {
		t.Fatalf("records = %+v, want [session, model]", got.Records)
	}
	if got.HasMore {
		t.Error("has_more = true, want false")
	}
}

// TestHandleJournal_ResidentButNeverPersisted_EmptyPage covers the one race
// window a real never-persisted-but-resident session can occur through (see
// Server.lookup's own doc comment on the child-adoption race, and
// engine.LoadJournal's identical fs.ErrNotExist contract): a session known
// to this process (resident, s.lookup succeeds) whose log file does not
// exist on disk yet must answer 200 with an empty page, never a 404 or a
// 500 — the session genuinely exists, it simply has no durable records.
// This constructs that state directly (white-box, same package) rather than
// racing handleCreate's own eager Persist call, which makes it
// unreachable through the ordinary HTTP surface in this test binary.
func TestHandleJournal_ResidentButNeverPersisted_EmptyPage(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	sess, err := h.srv.opts.NewSession(message.ModelRef{Provider: "test", Model: "m1"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	h.srv.mu.Lock()
	h.srv.sessions[sess.ID] = &sessionState{sess: sess, lastUsed: time.Now()}
	h.srv.mu.Unlock()

	resp, data := h.do("GET", "/session/"+sess.ID+"/journal", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	got := decodeJournal(t, data)
	if len(got.Records) != 0 {
		t.Errorf("records = %+v, want empty", got.Records)
	}
	if got.HasMore {
		t.Error("has_more = true, want false")
	}
}

// TestHandleJournal_ReturnsRecordsOldestFirst drives one real prompt turn
// and checks the journal reports the session header, model, and both
// messages in oldest-first order with ascending Seq — the shape a debugging
// client actually walks.
func TestHandleJournal_ReturnsRecordsOldestFirst(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("hi there"),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	resp, data = h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait status %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("GET", "/session/"+id+"/journal", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("journal status %d: %s", resp.StatusCode, data)
	}
	got := decodeJournal(t, data)
	if len(got.Records) != 4 {
		t.Fatalf("records = %+v, want 4 (session, model, user message, assistant message)", got.Records)
	}
	for i, r := range got.Records {
		if r.Seq != i+1 {
			t.Errorf("records[%d].Seq = %d, want %d", i, r.Seq, i+1)
		}
	}
	if got.Records[0].Type != "session" {
		t.Errorf("records[0].Type = %q, want session", got.Records[0].Type)
	}
	if got.NextCursor != got.Records[len(got.Records)-1].Seq {
		t.Errorf("next_cursor = %d, want %d (last record's seq)", got.NextCursor, got.Records[len(got.Records)-1].Seq)
	}
	if got.HasMore {
		t.Error("has_more = true, want false: the whole log fit in one page")
	}
}

// TestHandleJournal_Pagination proves `from`/`limit` page through a longer
// journal exactly like the SSE stream's own `from` cursor: a first page
// capped at limit=2 reports has_more=true and a next_cursor a follow-up
// request can resume from, converging on every record exactly once with no
// gaps or repeats.
func TestHandleJournal_Pagination(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("one"),
		asstTurn("two"),
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	for _, text := range []string{"first", "second"} {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": text}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt(%s) status %d: %s", text, resp.StatusCode, data)
		}
		resp, data = h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("wait status %d: %s", resp.StatusCode, data)
		}
	}

	resp, data := h.do("GET", "/session/"+id+"/journal", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("journal status %d: %s", resp.StatusCode, data)
	}
	full := decodeJournal(t, data)
	if len(full.Records) < 3 {
		t.Fatalf("full journal too short to page through: %+v", full.Records)
	}

	var paged []engine.JournalRecord
	from := 0
	for i := 0; i < 100; i++ { // bounded loop: a stuck cursor must not hang the test
		resp, data := h.do("GET", "/session/"+id+"/journal?from="+itoa(from)+"&limit=2", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("journal page status %d: %s", resp.StatusCode, data)
		}
		page := decodeJournal(t, data)
		if len(page.Records) == 0 {
			break
		}
		paged = append(paged, page.Records...)
		from = page.NextCursor
		if !page.HasMore {
			break
		}
	}
	if len(paged) != len(full.Records) {
		t.Fatalf("paged %d records, want %d", len(paged), len(full.Records))
	}
	for i := range full.Records {
		if paged[i].Seq != full.Records[i].Seq || paged[i].Type != full.Records[i].Type {
			t.Errorf("paged[%d] = {seq=%d type=%q}, want {seq=%d type=%q}",
				i, paged[i].Seq, paged[i].Type, full.Records[i].Seq, full.Records[i].Type)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
