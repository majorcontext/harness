package server

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// poisonReasoningTurn is one scripted worker turn whose assistant message
// carries a Reasoning part with a hand-set, non-zero-length but invalid
// ProviderData entry. This bypasses message.Normalize entirely: Normalize
// (the ingest choke point every message passes through, see
// engine.Session.append) only deletes ZERO-length ProviderData entries — it
// never validates a present entry's JSON, exactly like the ToolCall.Arguments
// footgun it *does* guard against. So an assistant message built this way
// (as a real provider adapter well might, on a stream that dies mid
// thinking-block) reaches session history unmodified and later fails to
// marshal with "json: error calling MarshalJSON for type message.Parts" —
// the exact failure observed in production on
// ses_01kx453ewfedqrg7p3c64f8sca / ses_01kx453ev9ejattygpf7rbzptw.
func poisonReasoningTurn(id string) []provider.Event {
	msg := &message.Message{
		ID:   id,
		Role: message.RoleAssistant,
		Parts: message.Parts{
			&message.Reasoning{
				ProviderData: message.ProviderData{
					// Non-empty but truncated/invalid JSON: passes every
					// len()==0 guard (Normalize, ProviderData.MarshalJSON,
					// ProviderData.Get) and only fails once encoding/json
					// tries to compact it inside a larger document.
					"anthropic": json.RawMessage(`{"signature":"trunc`),
				},
			},
		},
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
// WHOLESALE today because one resident message (a poisoned Reasoning part)
// failed json.Marshal, taking down the entire transcript view exactly when
// it was most needed to diagnose the death. The handler must marshal
// per-message, substituting a {id, role, marshal_error} placeholder for any
// message that fails, and still return 200 with every healthy message intact.
func TestGetMessagesDegradesPoisonMessageInsteadOf500(t *testing.T) {
	prov := &scriptedProvider{
		name: "test",
		turns: [][]provider.Event{
			asstTurn("first reply"),
			poisonReasoningTurn("msg_poison"),
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
