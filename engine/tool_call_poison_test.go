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
// turn persists cleanly and the reloaded log matches in-memory history
// exactly.
//
// # NEP-5272 update, then superseded: StopMaxTokens-with-a-ToolCall now
// drops the call outright
//
// StopMaxTokens-with-a-ToolCall was, for a time, exactly the shape
// appendUnexecutedToolCallResults exists for (see its doc comment and
// unexecutedToolCallStopReasonTextFmt's incident writeup): the tool name and
// call ID were kept, Arguments cleared to empty, and a synthetic is_error
// tool-role result appended for it. A later adversarial review of the
// max_tokens auto-continue feature (PR #193) found that shape still
// replayed a hollow, argument-less tool_use the model never actually
// finished emitting — indistinguishable, once the real Arguments are gone,
// from a call the model genuinely intended to make with no arguments. The
// design changed again: dropInvalidPartialToolCall (engine.go) now removes
// a StopMaxTokens turn's TRAILING ToolCall part entirely, in place, before
// it is ever appended to history, when its Arguments do not parse as valid
// JSON — see that function's own doc comment. No synthetic result is
// generated for a dropped call either: there is no usable intent left to
// report as failed. Both changes are compatible with THIS test's actual
// point (the marshal-failure incident): dropping the part before append
// means Normalize's own invalid-Arguments clearing (still exercised by any
// OTHER producer that bypasses this engine-level guard, per its own doc
// comment) never even needs to run for this path, and persisting still
// never fails.
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

	// The invalid partial call is gone entirely -- see
	// dropInvalidPartialToolCall's doc comment: a genuinely mid-emission
	// call carries no usable intent, so it is dropped outright rather than
	// kept with its Arguments cleared. The assistant's other content (the
	// "running" text it managed to emit before the cutoff) survives; no
	// synthetic tool-role result is generated for the dropped call, so
	// history ends on the assistant message itself.
	h := s.History()
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2 (user, assistant(text only, partial tool call dropped)): %+v", len(h), h)
	}
	assistant := h[1]
	if assistant.Role != message.RoleAssistant {
		t.Fatalf("h[1].Role = %s, want assistant", assistant.Role)
	}
	if got := assistant.Parts.Text(); got != "running" {
		t.Errorf("assistant text = %q, want %q (unaffected by dropping the tool call)", got, "running")
	}
	for _, p := range assistant.Parts {
		if tc, ok := p.(*message.ToolCall); ok {
			t.Errorf("assistant message still carries a ToolCall part %+v, want the invalid partial call dropped entirely", tc)
		}
	}
	if got := s.toolExecutions(); got != 0 {
		t.Errorf("toolExecutions() = %d, want 0 (a truncated call must never actually run)", got)
	}

	// The session log is loadable and agrees with in-memory history — the
	// turn that used to fail persist ("never journaled") is now durable.
	// The resident history is already orphan-free by this point
	// (appendUnexecutedToolCallResults paired tc1 immediately, live), so
	// the comparison is against s.History() raw — wrapping it in
	// ResolveOrphanToolCalls here would repair both sides identically and
	// let a future regression that left a resident orphan pass unnoticed.
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
