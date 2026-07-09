package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestPersistTruncatedToolCallArguments reproduces the incident behind two
// goal sessions observed in production, ses_01kx453ewfedqrg7p3c64f8sca and
// ses_01kx453ev9ejattygpf7rbzptw: both died at the start of a worker turn
// with "json: error calling MarshalJSON for type json.RawMessage:
// unexpected end of JSON input", three identical attempts, and
// GET /session/{id}/message on them returned 500 with "MarshalJSON for
// type message.Parts" — while the on-disk log stayed clean, because the
// poisoned assistant message failed to persist and was never journaled.
//
// Every existing guard at the time (ToolCall.safeArguments,
// ProviderData.Get/MarshalJSON) special-cased len(Arguments) == 0 only.
// The actual trigger is a provider stream that dies mid tool_use block —
// a connection drop during input_json_delta accumulation, or (as audited in
// provider/anthropic/anthropic.go) Anthropic's own protocol emitting
// content_block_stop/message_delta(stop_reason: max_tokens)/message_stop
// for a tool_use block truncated by the token budget — leaving
// ToolCall.Arguments holding non-empty but syntactically invalid JSON. That
// value sails past every len==0 guard, and json.RawMessage.MarshalJSON does
// not validate its bytes: the failure only surfaces once the value is
// embedded in a larger document and encoding/json compacts it to validate,
// at the two sites the incident hit — engine.Session.append's
// persistMessage (the "worker turn died" symptom) and server's
// GET /session/{id}/message, which simply re-marshals the same resident
// s.history (the "message.Parts 500" symptom).
//
// This scripted provider models the assembled shape a real stream would
// hand back rather than replaying raw SSE bytes (the fake/stub stream the
// fix description asks for): the assistant message it emits already
// carries a ToolCall whose Arguments is the truncated tail of a JSON object
// that generation stopped mid-way through, tagged with StopMaxTokens
// (not StopToolUse) to match the real Anthropic wire shape for exactly this
// case — the model never got to finish emitting the tool call, so the API
// never reports tool_use, and the engine never attempts to execute it.
//
// Before the fix (message.Message.Normalize dropping invalid
// ToolCall.Arguments at the engine's one append/ingest choke point, plus
// safeArguments' own defense-in-depth): PersistErr is non-nil with exactly
// the production error text, and json.Marshal(s.History()) — what
// GET /message does — fails mentioning message.Parts. After the fix: the
// turn persists cleanly, the tool name and call ID survive (only the
// unusable truncated arguments are dropped, replaced with an empty object —
// the same normalization already applied to a legitimately empty
// Arguments), the reloaded log matches in-memory history exactly, and a
// following worker turn — which now transcodes a clean history — succeeds
// instead of dying identically on every retry.
func TestPersistTruncatedToolCallArguments(t *testing.T) {
	dir := t.TempDir()
	truncated := toolCall("tc1", "bash", `{"command":"echo hel`) // cut off mid-argument, non-empty, invalid JSON
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "running"}, truncated),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt (poisoned turn): %v", err)
	}

	// Symptom (a): persisting the poisoned assistant message must not fail
	// — the production "died at the start of a worker turn" error.
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v, want nil", err)
	}

	// Symptom (b): GET /session/{id}/message marshals the resident history
	// directly (server/handlers.go's handleMessages) — it must never fail
	// on a ToolCall's Arguments, poisoned or not.
	if _, err := json.Marshal(s.History()); err != nil {
		t.Fatalf("json.Marshal(History()) = %v, want success (this is what GET /message does)", err)
	}

	// The tool call's identity survives; only the unusable truncated
	// arguments are gone, normalized the same way empty Arguments already
	// are (see ToolCall.safeArguments) rather than dropping the whole part
	// and losing which tool the model was calling.
	h := s.History()
	var found *message.ToolCall
	for _, p := range h[len(h)-1].Parts {
		if tc, ok := p.(*message.ToolCall); ok {
			found = tc
		}
	}
	if found == nil {
		t.Fatalf("assistant message lost its ToolCall part entirely: %+v", h[len(h)-1])
	}
	if found.CallID != "tc1" || found.Name != "bash" {
		t.Errorf("ToolCall identity not preserved: %+v", found)
	}
	if len(found.Arguments) != 0 {
		t.Errorf("ToolCall.Arguments = %s, want cleared (truncated JSON is unusable)", found.Arguments)
	}

	// The session log is loadable and agrees with in-memory history — the
	// turn that used to fail persist ("never journaled") is now durable.
	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got, want := historyJSON(t, loaded.History()), historyJSON(t, s.History()); got != want {
		t.Errorf("loaded history = %s\nwant %s", got, want)
	}

	// The session is not wedged: unlike the incident (three identical
	// failures because every retry re-transcoded the same poisoned
	// history), the next worker turn — which now builds its request from a
	// clean history — succeeds.
	final, err := s.Prompt(context.Background(), "continue")
	if err != nil {
		t.Fatalf("second Prompt (subsequent worker turn) = %v, want success", err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("second Prompt final = %q, want %q", final.Parts.Text(), "done")
	}
	// The request the second turn actually built — s.History() as of that
	// call, the exact value a real transcoder marshals into the wire body
	// (see provider/anthropic/transcode.go, provider/openaicompat/transcode.go)
	// — must itself be marshalable: this is the "request build" half of the
	// incident, reproduced without depending on any one provider's wire
	// shape.
	if len(prov.requests) < 2 {
		t.Fatalf("provider recorded %d requests, want at least 2", len(prov.requests))
	}
	if _, err := json.Marshal(prov.requests[1].Messages); err != nil {
		t.Fatalf("json.Marshal(second request's Messages) = %v, want success", err)
	}
}
