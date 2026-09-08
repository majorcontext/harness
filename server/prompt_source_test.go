package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestParsePromptProvenanceDefaultsToAPI is the named-failure test for
// parsePromptProvenance's default rule: a caller that names no source at
// all must resolve to an unrejected, zero-Source PromptProvenance (which
// every enqueue path then Normalizes to message.PromptSourceAPI) — never
// message.PromptSourceTyped. A regression that defaults an absent source
// to "typed" would misattribute every unlabeled caller (a script, an
// unlabeled integration) as a live human.
func TestParsePromptProvenanceDefaultsToAPI(t *testing.T) {
	prov, code, err := parsePromptProvenance(promptSourceInput{})
	if err != nil || code != 0 {
		t.Fatalf("parsePromptProvenance({}) = (%+v, %d, %v), want no error", prov, code, err)
	}
	if got := prov.Normalized().Source; got != message.PromptSourceAPI {
		t.Errorf("Normalized().Source = %q, want %q", got, message.PromptSourceAPI)
	}
}

// TestParsePromptProvenanceAcceptsCallerSuppliableSources checks every
// source value a caller may legitimately assert reaches
// engine.PromptProvenance unchanged, alongside SourceID/SourceLabel.
func TestParsePromptProvenanceAcceptsCallerSuppliableSources(t *testing.T) {
	for _, src := range []message.PromptSource{
		message.PromptSourceTyped, message.PromptSourceAPI,
		message.PromptSourceSchedule, message.PromptSourceCrossBox,
	} {
		prov, code, err := parsePromptProvenance(promptSourceInput{
			Source: string(src), SourceID: "id-1", SourceLabel: "label-1",
		})
		if err != nil || code != 0 {
			t.Fatalf("parsePromptProvenance(%q) = (%+v, %d, %v), want no error", src, prov, code, err)
		}
		want := engine.PromptProvenance{Source: src, SourceID: "id-1", SourceLabel: "label-1"}
		if prov != want {
			t.Errorf("parsePromptProvenance(%q) = %+v, want %+v", src, prov, want)
		}
	}
}

// TestParsePromptProvenanceRejectsTask is the named-failure test for the
// one value a caller must never assert: message.PromptSourceTask names
// this engine's own internal task-tool relay
// (SessionManager.SendToDescendant), which no HTTP caller reaches through
// these routes — a caller asserting it must get a 400, not a silently
// accepted, misleading provenance tag.
func TestParsePromptProvenanceRejectsTask(t *testing.T) {
	_, code, err := parsePromptProvenance(promptSourceInput{Source: string(message.PromptSourceTask)})
	if err == nil {
		t.Fatal("parsePromptProvenance(task) = nil error, want a rejection")
	}
	if code != http.StatusBadRequest {
		t.Errorf("code = %d, want %d", code, http.StatusBadRequest)
	}
}

// TestParsePromptProvenanceRejectsUnknownSource guards against a typo or a
// forward-incompatible client silently landing an unrecognized source.
func TestParsePromptProvenanceRejectsUnknownSource(t *testing.T) {
	_, code, err := parsePromptProvenance(promptSourceInput{Source: "bogus"})
	if err == nil {
		t.Fatal("parsePromptProvenance(bogus) = nil error, want a rejection")
	}
	if code != http.StatusBadRequest {
		t.Errorf("code = %d, want %d", code, http.StatusBadRequest)
	}
}

// TestEnqueueProvenanceExposedOnQueueGet is the end-to-end, named-failure
// test for Task 3's contract: a caller (the boxes control plane's
// schedule_task/cron lifecycle worker, notably) POSTs
// /session/{id}/enqueue with source="schedule" plus source_id/
// source_label, and that provenance must be journaled and readable back
// via GET /session/{id}/queue — not silently dropped. Mirrors
// TestQueueGetReturnsWatermarkAndPending's exact busy-then-enqueue
// technique so the entry stays pending (never solo-dispatched) long
// enough to inspect.
func TestEnqueueProvenanceExposedOnQueueGet(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("occupant done")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "occupant"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("occupant prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/enqueue", map[string]any{
		"parts":        []map[string]string{{"type": "text", "text": "scheduled follow-up"}},
		"seq":          int64(4),
		"source":       "schedule",
		"source_id":    "sched_123",
		"source_label": "nightly CI check",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("GET", "/session/"+id+"/queue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET queue status %d: %s", resp.StatusCode, data)
	}
	var q queueGetResponse
	if err := json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if len(q.Queued) != 1 {
		t.Fatalf("queue read = %+v, want exactly one queued entry", q)
	}
	got := q.Queued[0]
	if got.Source != "schedule" || got.SourceID != "sched_123" || got.SourceLabel != "nightly CI check" {
		t.Fatalf("queued[0] provenance = %+v, want source=schedule source_id=sched_123 source_label=%q",
			got, "nightly CI check")
	}

	close(prov.release)
	h.waitIdle(id)
}

// TestEnqueueRejectsReservedSource proves the HTTP layer surfaces
// parsePromptProvenance's rejection as a 400, not a silently-accepted
// enqueue.
func TestEnqueueRejectsReservedSource(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/enqueue", map[string]any{
		"parts":  []map[string]string{{"type": "text", "text": "hi"}},
		"seq":    int64(1),
		"source": "task",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enqueue with source=task status %d: %s, want 400", resp.StatusCode, data)
	}
}

// TestSanitizeSourceIDRejectsOversizeAndNonPrintable is the named-failure
// test for sanitizeSourceID's two rejection rules: an id over
// sourceIDMaxBytes, and an id containing a byte outside printable ASCII
// (0x20-0x7E) — a newline or a raw control byte a caller (accidentally or
// not) puts in what is supposed to be a short machine identifier. Both
// must be REJECTED, not silently truncated or stripped: a truncated or
// byte-mangled id looks up nothing, or the wrong thing, later.
func TestSanitizeSourceIDRejectsOversizeAndNonPrintable(t *testing.T) {
	if _, err := sanitizeSourceID(strings.Repeat("a", sourceIDMaxBytes+1)); err == nil {
		t.Errorf("sanitizeSourceID(%d bytes) = nil error, want a rejection (max %d)", sourceIDMaxBytes+1, sourceIDMaxBytes)
	}
	if _, err := sanitizeSourceID("sched_123\nrm -rf /"); err == nil {
		t.Error("sanitizeSourceID with an embedded newline = nil error, want a rejection")
	}
	if got, err := sanitizeSourceID("sched_123"); err != nil || got != "sched_123" {
		t.Errorf("sanitizeSourceID(%q) = (%q, %v), want (%q, nil)", "sched_123", got, err, "sched_123")
	}
	if got, err := sanitizeSourceID(""); err != nil || got != "" {
		t.Errorf("sanitizeSourceID(\"\") = (%q, %v), want (\"\", nil)", got, err)
	}
}

// TestSanitizeSourceLabelBoundsAndStripsControlChars is the named-failure
// test for sanitizeSourceLabel's repair rules: a label over
// sourceLabelMaxBytes must be truncated (never rejected — it is display-
// only text), and an embedded control character (a newline, an ANSI
// escape byte) must be stripped, not merely passed through into a
// durably-journaled, rendered field. Invalid UTF-8 is the one case that
// IS rejected: there is no well-defined repair for malformed encoding.
func TestSanitizeSourceLabelBoundsAndStripsControlChars(t *testing.T) {
	huge := strings.Repeat("x", sourceLabelMaxBytes+100)
	got, err := sanitizeSourceLabel(huge)
	if err != nil {
		t.Fatalf("sanitizeSourceLabel(huge) error = %v, want no error (truncate, don't reject)", err)
	}
	if len(got) > sourceLabelMaxBytes {
		t.Errorf("sanitizeSourceLabel(huge) len = %d, want <= %d", len(got), sourceLabelMaxBytes)
	}

	got, err = sanitizeSourceLabel("nightly CI check\x1b[31m\ninjected\x00")
	if err != nil {
		t.Fatalf("sanitizeSourceLabel with control chars error = %v, want no error (strip, don't reject)", err)
	}
	if strings.ContainsAny(got, "\x1b\n\x00") {
		t.Errorf("sanitizeSourceLabel = %q, want every control character stripped", got)
	}
	if want := "nightly CI check[31minjected"; got != want {
		t.Errorf("sanitizeSourceLabel = %q, want %q", got, want)
	}

	if _, err := sanitizeSourceLabel("bad utf8: \xff\xfe"); err == nil {
		t.Error("sanitizeSourceLabel with invalid UTF-8 = nil error, want a rejection")
	}
}

// TestEnqueueRejectsOversizeSourceID is the end-to-end HTTP counterpart:
// an enqueue request whose source_id exceeds sourceIDMaxBytes must 400,
// not be silently truncated and journaled.
func TestEnqueueRejectsOversizeSourceID(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/enqueue", map[string]any{
		"parts":     []map[string]string{{"type": "text", "text": "hi"}},
		"seq":       int64(1),
		"source":    "schedule",
		"source_id": strings.Repeat("a", sourceIDMaxBytes+1),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enqueue with an oversize source_id status %d: %s, want 400", resp.StatusCode, data)
	}
}

// TestPromptAsyncIdleDispatchCarriesProvenanceOnMessage is the named-
// failure test for the solo-dispatch provenance gap: a prompt_async
// request that dispatches AT ONCE (the session is idle, never queued at
// all) used to record its source/source_id/source_label nowhere — only a
// prompt that happened to land behind a busy turn got its provenance
// journaled, on the queue entry. Attribution must not depend on whether
// the box happened to be busy: this drives prompt_async against an IDLE
// session and asserts the appended message itself (GET /session/{id}/
// message) carries the SAME provenance a queued caller would get on its
// OperatorBatchEntry.
func TestPromptAsyncIdleDispatchCarriesProvenanceOnMessage(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("ack")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts":        []map[string]string{{"type": "text", "text": "nightly sweep"}},
		"source":       "schedule",
		"source_id":    "sched_789",
		"source_label": "nightly sweep",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	resp, data = h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
	}
	var msgs []struct {
		Source      string `json:"source"`
		SourceID    string `json:"source_id"`
		SourceLabel string `json:"source_label"`
		Parts       []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v: %s", err, data)
	}
	var found bool
	for _, m := range msgs {
		if len(m.Parts) > 0 && m.Parts[0].Text == "nightly sweep" {
			found = true
			if m.Source != "schedule" || m.SourceID != "sched_789" || m.SourceLabel != "nightly sweep" {
				t.Fatalf("solo-dispatched message provenance = %+v, want source=schedule source_id=sched_789 source_label=%q",
					m, "nightly sweep")
			}
		}
	}
	if !found {
		t.Fatalf("no message carries the dispatched prompt text: %s", data)
	}
}

// TestSessionSendCarriesProvenanceOnMessage is session.send's counterpart
// to TestPromptAsyncIdleDispatchCarriesProvenanceOnMessage: POST
// /session/{id}/send also accepts source/source_id/source_label (this
// PR's own change), and its solo-dispatched message must carry them too.
func TestSessionSendCarriesProvenanceOnMessage(t *testing.T) {
	prov := &scriptedProvider{name: "root", turns: [][]provider.Event{asstTurn("hello back")}}
	h := multiProviderHarness(t, message.ModelRef{Provider: "root", Model: "m1"}, nil, prov)

	resp, data := h.do("POST", "/session", map[string]string{"model": "root/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}
	var root struct {
		ID string `json:"id"`
	}
	mustUnmarshal(t, data, &root)

	resp, data = h.do("POST", "/session/"+root.ID+"/send", map[string]any{
		"text":         "cross-box relay",
		"source":       "cross_box",
		"source_id":    "box-42",
		"source_label": "relay from box-42",
	})
	if resp.StatusCode != 202 {
		t.Fatalf("send status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(root.ID)

	resp, data = h.do("GET", "/session/"+root.ID+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get messages status %d: %s", resp.StatusCode, data)
	}
	var msgs []struct {
		Source      string `json:"source"`
		SourceID    string `json:"source_id"`
		SourceLabel string `json:"source_label"`
		Parts       []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v: %s", err, data)
	}
	var found bool
	for _, m := range msgs {
		if len(m.Parts) > 0 && m.Parts[0].Text == "cross-box relay" {
			found = true
			if m.Source != "cross_box" || m.SourceID != "box-42" || m.SourceLabel != "relay from box-42" {
				t.Fatalf("session.send message provenance = %+v, want source=cross_box source_id=box-42 source_label=%q",
					m, "relay from box-42")
			}
		}
	}
	if !found {
		t.Fatalf("no message carries the sent text: %s", data)
	}
}
