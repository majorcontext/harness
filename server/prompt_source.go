package server

import (
	"fmt"
	"net/http"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// promptSourceInput is the wire shape a caller uses to name who/what is
// enqueuing a prompt — see message.PromptSource's own doc comment for the
// values and message.OperatorBatchEntry for where this rides once an
// operator-batch drain exposes it. Embedded by every enqueue-capable
// request body (prompt_async, enqueue, session.send) so all three share
// one parser (parsePromptProvenance).
type promptSourceInput struct {
	// Source names who/what is enqueuing this prompt: "typed" (a live
	// human), "api" (a generic programmatic caller — the default when
	// this is omitted), "schedule" (a schedule/cron delivery), or
	// "cross_box" (relayed from another box). "task" is rejected: it
	// names this engine's own internal task-tool relay, which no HTTP
	// caller reaches through these routes.
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
// subset of message.PromptSource and returns the engine.PromptProvenance
// an enqueue call should record. An empty Source is accepted and left
// unnormalized here — engine.PromptProvenance.Normalized (called by every
// enqueue path this feeds) is what turns it into message.PromptSourceAPI;
// this function's own job is only to reject a Source no caller may assert.
func parsePromptProvenance(in promptSourceInput) (engine.PromptProvenance, int, error) {
	switch message.PromptSource(in.Source) {
	case "", message.PromptSourceTyped, message.PromptSourceAPI, message.PromptSourceSchedule, message.PromptSourceCrossBox:
		return engine.PromptProvenance{
			Source:      message.PromptSource(in.Source),
			SourceID:    in.SourceID,
			SourceLabel: in.SourceLabel,
		}, 0, nil
	case message.PromptSourceTask:
		return engine.PromptProvenance{}, http.StatusBadRequest,
			fmt.Errorf("source %q is reserved for the engine's own task-tool relay", in.Source)
	default:
		return engine.PromptProvenance{}, http.StatusBadRequest, fmt.Errorf("unknown source %q", in.Source)
	}
}
