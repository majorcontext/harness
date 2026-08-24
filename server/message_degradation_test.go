package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// poisonMessageTurn is one scripted worker turn whose assistant message
// carries a CreatedAt timestamp with a year outside encoding/json's
// supported range for time.Time ([0,9999] — see the stdlib's
// "Time.MarshalJSON: year outside of range [0,9999]"), so json.Marshal of
// the message fails deterministically. This poison is deliberately NOT a
// message-package footgun: CreatedAt is a plain time.Time field message.go
// applies no validation or guard to, so it stays a reliable way to force a
// genuine marshal failure regardless of how thoroughly this package's own
// JSON guards (ToolCall.safeArguments, ProviderData.MarshalJSON, Normalize)
// are hardened.
//
// An earlier version of this poison instead set a non-empty-but-invalid
// Reasoning.ProviderData entry, reproducing the exact mechanism behind
// production incident ses_01kx453ewfedqrg7p3c64f8sca /
// ses_01kx453ev9ejattygpf7rbzptw — "passes every len()==0 guard (Normalize,
// ProviderData.MarshalJSON, ProviderData.Get) and only fails once
// encoding/json tries to compact it inside a larger document." That whole
// class of failure was closed by extending ProviderData.MarshalJSON and
// Normalize to also reject a non-empty-but-syntactically-invalid entry,
// exactly mirroring the ToolCall.Arguments guard they were already modeled
// on (see message.Message.Normalize's doc comment, "A ProviderData entry
// has the exact same invalid-but-non-empty footgun") — so that mechanism no
// longer produces a marshal failure and can no longer serve as poison here;
// this test's OWN purpose (GET /session/{id}/message degrades a
// marshal-failing resident message instead of 500ing the whole response)
// is orthogonal to that fix and still needs some reliable way to force a
// failure, hence the switch to an out-of-range CreatedAt.
func poisonMessageTurn(id string) []provider.Event {
	msg := &message.Message{
		ID:   id,
		Role: message.RoleAssistant,
		Parts: message.Parts{
			&message.Text{Text: "poisoned"},
		},
		// Year 10000 is one past time.Time.MarshalJSON's supported range.
		CreatedAt: time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}
}

// messagePlaceholderForTest mirrors the placeholder object handleMessages
// substitutes for an unmarshalable resident message.
type messagePlaceholderForTest struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	MarshalError string `json:"marshal_error"`
}

// TestGetMessagesDegradesPoisonMessageInsteadOf500 is the red-first
// regression test for the incident: GET /session/{id}/message 500'd
// WHOLESALE today because one resident message (a poisoned message)
// failed json.Marshal, taking down the entire transcript view exactly when
// it was most needed to diagnose the death. The handler must marshal
// per-message, substituting a {id, role, marshal_error} placeholder for any
// message that fails, and still return 200 with every healthy message intact.
func TestGetMessagesDegradesPoisonMessageInsteadOf500(t *testing.T) {
	prov := &scriptedProvider{
		name: "test",
		turns: [][]provider.Event{
			asstTurn("first reply"),
			poisonMessageTurn("msg_poison"),
			asstTurn("third reply"),
		},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	for _, text := range []string{"one", "two", "three"} {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": text}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt(%s) status %d: %s", text, resp.StatusCode, data)
		}
		resp, data = h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("wait until=idle status %d: %s", resp.StatusCode, data)
		}
	}

	// Sanity: a naive whole-slice json.Marshal of this history really does
	// fail — proving the poison is genuine, not an artifact of the test.
	history := sessionHistoryForTest(t, h, id)
	if _, err := json.Marshal(history); err == nil {
		t.Fatal("expected whole-slice marshal of this history to fail; poison did not take")
	}

	resp, data := h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /message status %d, want 200 (must degrade, not 500): %s", resp.StatusCode, data)
	}

	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		t.Fatalf("response body not a JSON array: %v: %s", err, data)
	}
	// 3 user + 3 assistant = 6 total, one of which (the poisoned assistant
	// reply) must come back as a placeholder.
	if len(raws) != 6 {
		t.Fatalf("got %d messages, want 6: %s", len(raws), data)
	}

	var placeholders int
	var healthy int
	for _, raw := range raws {
		var ph messagePlaceholderForTest
		if err := json.Unmarshal(raw, &ph); err == nil && ph.MarshalError != "" {
			placeholders++
			if ph.ID != "msg_poison" {
				t.Errorf("placeholder id = %q, want msg_poison", ph.ID)
			}
			if ph.Role != "assistant" {
				t.Errorf("placeholder role = %q, want assistant", ph.Role)
			}
			continue
		}
		var m message.Message
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("healthy entry failed to unmarshal as message.Message: %v: %s", err, raw)
			continue
		}
		healthy++
	}
	if placeholders != 1 {
		t.Errorf("placeholders = %d, want 1", placeholders)
	}
	if healthy != 5 {
		t.Errorf("healthy messages = %d, want 5", healthy)
	}
}

// sessionHistoryForTest reaches into the resident session to read its raw
// history (bypassing the HTTP layer) so the test can independently confirm
// the poison actually breaks a whole-slice marshal.
func sessionHistoryForTest(t *testing.T, h *harness, id string) []message.Message {
	t.Helper()
	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatalf("session %s not resident", id)
	}
	return st.sess.History()
}
