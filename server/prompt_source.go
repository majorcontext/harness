package server

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// sourceIDMaxBytes and sourceLabelMaxBytes bound the two free-form
// provenance fields a caller supplies alongside `source` — see
// promptSourceInput's own doc comment. Both are journaled (durably, on
// the prompt.queued record) and re-exposed on GET /session/{id}/queue and
// on a later batch drain's OperatorBatchEntry, so an unbounded value is a
// durable amplification, not merely an oversized single request: a large
// source_label would be copied again into every OperatorBatchEntry of a
// batch that happens to fold it in. server/AGENTS.md's own precedent
// ("Bound and validate X-Request-Id before logging it") is a caller-
// controlled identifier the server must not trust at face value; these
// two fields are the same shape.
const (
	sourceIDMaxBytes    = 128
	sourceLabelMaxBytes = 256
)

// sanitizeSourceID rejects in when it exceeds sourceIDMaxBytes or contains
// a byte outside printable ASCII (0x20-0x7E) — unlike sourceLabel below,
// an identifier is silently truncated or stripped at the caller's own
// peril (a truncated or byte-mangled id looks up nothing, or the wrong
// thing, later), so this REJECTS a malformed one rather than repairing it.
// Empty is always valid (SourceID is optional).
func sanitizeSourceID(in string) (string, error) {
	if len(in) > sourceIDMaxBytes {
		return "", fmt.Errorf("source_id exceeds %d bytes", sourceIDMaxBytes)
	}
	for i := 0; i < len(in); i++ {
		if c := in[i]; c < 0x20 || c > 0x7e {
			return "", fmt.Errorf("source_id must be printable ASCII")
		}
	}
	return in, nil
}

// sanitizeSourceLabel bounds and cleans in for durable storage and
// display — see promptSourceInput's own doc comment: SourceLabel is
// "free-form, human-readable... display only, never parsed," so unlike
// sourceID above this REPAIRS a merely-too-long value (truncates to
// sourceLabelMaxBytes, at a valid rune boundary — never splits a
// multi-byte UTF-8 sequence) and strips C0 (0x00-0x1F, 0x7F) and C1
// (0x80-0x9F) control characters (a newline or an ANSI escape sequence
// injected into a rendered console bubble), plus the Unicode bidi
// override and zero-width characters below, rather than rejecting the
// whole request over them. Invalid UTF-8 IS rejected, not repaired: there
// is no well-defined truncation or per-byte strip that recovers a
// caller's intended text from malformed encoding, so this returns an
// error instead of guessing.
func sanitizeSourceLabel(in string) (string, error) {
	if !utf8.ValidString(in) {
		return "", fmt.Errorf("source_label is not valid UTF-8")
	}
	if len(in) > sourceLabelMaxBytes {
		in = in[:sourceLabelMaxBytes]
		// Trim back to the last complete rune: a byte-count truncation can
		// land mid-sequence, and utf8.ValidString above only guaranteed the
		// ORIGINAL string was valid, not this truncated prefix.
		for len(in) > 0 && !utf8.ValidString(in) {
			in = in[:len(in)-1]
		}
	}
	return strings.Map(func(r rune) rune {
		if r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		if isBidiOrZeroWidth(r) {
			return -1
		}
		return r
	}, in), nil
}

// isBidiOrZeroWidth reports whether r is a Unicode bidirectional-override
// or zero-width character — U+200E/U+200F (LRM/RLM), U+202A-U+202E
// (LRE/RLE/PDF/LRO/RLO), U+2066-U+2069 (LRI/RLI/FSI/PDI), or
// U+200B-U+200D/U+FEFF (ZWSP/ZWNJ/ZWJ/BOM). None of these carry a C0/C1
// byte, so the control-character strip above misses them, yet each can
// visually reorder or hide text in a rendered console bubble — the same
// hazard C0/C1 stripping exists to close.
func isBidiOrZeroWidth(r rune) bool {
	switch {
	case r == 0x200e || r == 0x200f:
	case r >= 0x202a && r <= 0x202e:
	case r >= 0x2066 && r <= 0x2069:
	case r >= 0x200b && r <= 0x200d:
	case r == 0xfeff:
	default:
		return false
	}
	return true
}

// promptSourceInput is the wire shape a caller uses to name who/what is
// enqueuing a prompt — see message.PromptSource's own doc comment
// (including its "Trust model" section) for the values and
// message.OperatorBatchEntry for where this rides once an operator-batch
// drain exposes it. Embedded by every enqueue-capable request body
// (prompt_async, enqueue, session.send) so all three share one parser
// (parsePromptProvenance).
//
// This request field is an UNVERIFIED CLAIM, not an authenticated fact:
// harness authenticates the HTTP caller (one bearer token, one trust
// level), never which specific value that caller asserts here. Anything
// holding the session's own token — including a delegated Claude Code CLI
// process running inside the box, which reaches this same route through
// that same token — can assert source=typed for text no human typed. A
// consumer must render every value as the caller's own claim, never as
// proof of its content's real origin.
type promptSourceInput struct {
	// Source names who/what is enqueuing this prompt: "typed" (a live
	// human — an unverified claim, see this type's own doc comment),
	// "api" (a generic programmatic caller — the default when this is
	// omitted), "schedule" (a schedule/cron delivery), or "cross_box"
	// (relayed from another box). "task" is rejected: it names this
	// engine's own internal task-tool relay, which no HTTP caller reaches
	// through these routes.
	Source string `json:"source"`
	// SourceID is a free-form identifier for Source's own instance (a
	// schedule/cron id, a calling box id) — optional, carried through
	// verbatim.
	SourceID string `json:"source_id"`
	// SourceLabel is a free-form, human-readable label for the same
	// instance (a schedule's own display name) — optional, display only.
	SourceLabel string `json:"source_label"`
}

// parsePromptProvenance validates in.Source against the caller-suppliable
// subset of message.PromptSource, bounds and sanitizes in.SourceID/
// in.SourceLabel (sanitizeSourceID/sanitizeSourceLabel above), and returns
// the engine.PromptProvenance an enqueue call should record. An empty
// Source is accepted and left unnormalized here — engine.PromptProvenance.
// Normalized (called by every enqueue path this feeds) is what turns it
// into message.PromptSourceAPI; this function's own job is only to reject
// a Source no caller may assert, or a SourceID/SourceLabel this durable,
// re-exposed field must not carry unbounded or control-character-laden.
func parsePromptProvenance(in promptSourceInput) (engine.PromptProvenance, int, error) {
	switch message.PromptSource(in.Source) {
	case "", message.PromptSourceTyped, message.PromptSourceAPI, message.PromptSourceSchedule, message.PromptSourceCrossBox:
		sourceID, err := sanitizeSourceID(in.SourceID)
		if err != nil {
			return engine.PromptProvenance{}, http.StatusBadRequest, err
		}
		sourceLabel, err := sanitizeSourceLabel(in.SourceLabel)
		if err != nil {
			return engine.PromptProvenance{}, http.StatusBadRequest, err
		}
		return engine.PromptProvenance{
			Source:      message.PromptSource(in.Source),
			SourceID:    sourceID,
			SourceLabel: sourceLabel,
		}, 0, nil
	case message.PromptSourceTask:
		return engine.PromptProvenance{}, http.StatusBadRequest,
			fmt.Errorf("source %q is reserved for the engine's own task-tool relay", in.Source)
	default:
		return engine.PromptProvenance{}, http.StatusBadRequest, fmt.Errorf("unknown source %q", in.Source)
	}
}
