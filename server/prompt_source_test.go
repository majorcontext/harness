package server

import (
	"encoding/json"
	"net/http"
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
