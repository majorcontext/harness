package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// userMessages filters GET /session/{id}/message's transcript down to the
// RoleUser entries, in order — the oracle every test in this file checks a
// prompt's resolved id against.
func (h *harness) userMessages(id string) []message.Message {
	h.t.Helper()
	resp, data := h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET message status %d: %s", resp.StatusCode, data)
	}
	var all []message.Message
	if err := json.Unmarshal(data, &all); err != nil {
		h.t.Fatalf("unmarshal transcript: %v (%s)", err, data)
	}
	var users []message.Message
	for _, m := range all {
		if m.Role == message.RoleUser {
			users = append(users, m)
		}
	}
	return users
}

// TestPromptAsyncUsesSuppliedMessageID is the RED test for the feature's
// core promise: a caller-supplied `id` on POST /session/{id}/prompt_async
// is used VERBATIM as the resulting user message's own ID — both in the
// synchronous response's message_id field and in the durable transcript —
// never replaced by a server mint.
func TestPromptAsyncUsesSuppliedMessageID(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	const supplied = "console-optimistic-1"
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
		"id":    supplied,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	var pr promptAsyncResponse
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.MessageID != supplied {
		t.Fatalf("response message_id = %q, want the supplied id %q", pr.MessageID, supplied)
	}

	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 {
		t.Fatalf("user messages = %d, want 1: %+v", len(users), users)
	}
	if users[0].ID != supplied {
		t.Fatalf("transcript user message id = %q, want the supplied id %q", users[0].ID, supplied)
	}
}

// TestPromptAsyncMintsOnReservedOrEmptyMessageID is the fail-safe-guard
// test: an empty id, or one beginning with a reserved provenance prefix
// (cmpsum_ or message.SyntheticOrphanIDPrefix), must NEVER be used
// verbatim — the server mints a fresh msg_-prefixed id instead, the
// response and transcript agree on that same minted value, and the
// prompt still succeeds (never rejected).
func TestPromptAsyncMintsOnReservedOrEmptyMessageID(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		explicit bool // whether to include "id" in the JSON body at all
	}{
		{name: "empty_id_field_present", id: "", explicit: true},
		{name: "reserved_compaction_prefix", id: "cmpsum_hijack", explicit: true},
		{name: "reserved_synthetic_orphan_prefix", id: message.SyntheticOrphanIDPrefix + "0-x", explicit: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
			h := newHarness(t, prov)
			id := h.createSession("test/m1")

			body := map[string]any{
				"parts": []map[string]string{{"type": "text", "text": "hello"}},
			}
			if c.explicit {
				body["id"] = c.id
			}
			resp, data := h.do("POST", "/session/"+id+"/prompt_async", body)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
			}
			var pr promptAsyncResponse
			if err := json.Unmarshal(data, &pr); err != nil {
				t.Fatal(err)
			}
			if pr.MessageID == c.id {
				t.Fatalf("response message_id = %q, want a freshly minted id, not the rejected supplied value %q", pr.MessageID, c.id)
			}
			if !strings.HasPrefix(pr.MessageID, "msg_") {
				t.Fatalf("response message_id = %q, want a msg_-prefixed minted id", pr.MessageID)
			}

			h.waitIdle(id)

			users := h.userMessages(id)
			if len(users) != 1 {
				t.Fatalf("user messages = %d, want 1: %+v", len(users), users)
			}
			if users[0].ID != pr.MessageID {
				t.Fatalf("transcript user message id = %q, want it to match the response's minted message_id %q", users[0].ID, pr.MessageID)
			}
		})
	}
}

// TestPromptAsyncNoSuppliedIDMintsLikeBefore is the backward-compat test: a
// caller that never names `id` at all (every existing caller, before this
// feature) still gets a working prompt with a server-minted id — the
// existing, unmodified default behavior.
func TestPromptAsyncNoSuppliedIDMintsLikeBefore(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	var pr promptAsyncResponse
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pr.MessageID, "msg_") {
		t.Fatalf("response message_id = %q, want a msg_-prefixed minted id", pr.MessageID)
	}

	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 || users[0].ID != pr.MessageID {
		t.Fatalf("user messages = %+v, want exactly one whose id matches response message_id %q", users, pr.MessageID)
	}
}

// TestQueuedPromptDeliversSuppliedMessageID is the busy-queue RED test: a
// prompt submitted while the session is already busy is durably enqueued
// (not run immediately), yet its caller-supplied id still survives to the
// eventual user message once the occupying turn finishes and the queue
// drains — proving EnqueuePrompt's queued-prompt record carries the id
// through dispatchQueueHead, not just the immediate-dispatch fast path.
func TestQueuedPromptDeliversSuppliedMessageID(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("second done")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
		"id":    "first-msg",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	const queuedID = "console-queued-2"
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
		"id":    queuedID,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" {
		t.Fatalf("second prompt response = %+v, want status=queued", qr)
	}
	if qr.MessageID != queuedID {
		t.Fatalf("queued response message_id = %q, want the supplied id %q, even though the prompt has not run yet", qr.MessageID, queuedID)
	}

	close(prov.release) // let the first turn finish, which drains the queue
	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 2 {
		t.Fatalf("user messages = %d, want 2: %+v", len(users), users)
	}
	if users[0].ID != "first-msg" {
		t.Fatalf("first user message id = %q, want %q", users[0].ID, "first-msg")
	}
	if users[1].ID != queuedID {
		t.Fatalf("second (queued) user message id = %q, want the supplied id %q — a busy-session queue must deliver the client's id, not mint a new one at drain time", users[1].ID, queuedID)
	}
}

// TestSessionSendUsesSuppliedMessageID mirrors
// TestPromptAsyncUsesSuppliedMessageID for POST /session/{id}/send: the
// root-session send path shares runPrompt/PromptWithOrigin with
// prompt_async (see sendTextToRoot), so it gets the same client-id
// treatment — verified independently here rather than assumed.
func TestSessionSendUsesSuppliedMessageID(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	const supplied = "console-send-1"
	resp, data := h.do("POST", "/session/"+id+"/send", map[string]any{
		"text": "hello via send",
		"id":   supplied,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("session.send status %d: %s", resp.StatusCode, data)
	}
	var sr struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.MessageID != supplied {
		t.Fatalf("response message_id = %q, want the supplied id %q", sr.MessageID, supplied)
	}

	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 || users[0].ID != supplied {
		t.Fatalf("user messages = %+v, want exactly one whose id is %q", users, supplied)
	}
}
