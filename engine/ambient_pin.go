package engine

import (
	"time"

	"github.com/majorcontext/harness/message"
)

// Ambient segment kinds. Each is pinned independently so one segment's
// change does not re-pin the others.
const (
	ambientKindProcess  = "process"
	ambientKindMCP      = "mcp"
	ambientKindGoal     = "goal"
	ambientKindIdentity = "identity"
	ambientKindTask     = "task_notification"
)

type ambientSegment struct {
	kind string
	text string
}

type ambientPin struct {
	kind string
	// at is len(history) when this pin was first rendered; replay restores
	// it to that slot.
	at int
	// msg is frozen at pin time so replay is byte-identical by construction.
	msg  message.Message
	text string
}

// pinAmbient appends a pin when seg differs from the newest pin of its kind.
// Comparing against that pin is what keeps the non-idempotent task-
// notification segment safe: a retried turn, or one that requeued its
// notifications, re-renders the same text and adds no second pin.
func (s *Session) pinAmbient(kind, seg string, at int) {
	if seg == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.ambientPins) - 1; i >= 0; i-- {
		if s.ambientPins[i].kind != kind {
			continue
		}
		if s.ambientPins[i].text == seg {
			return
		}
		break
	}
	s.ambientPins = append(s.ambientPins, ambientPin{
		kind: kind,
		at:   at,
		text: seg,
		msg: message.Message{
			ID:        newID("msg"),
			Role:      message.RoleUser,
			Parts:     message.Parts{&message.EngineContext{Text: seg}},
			CreatedAt: time.Now().UTC(),
		},
	})
}

// replayAmbientPins interleaves pins back into history at their pinned slots.
//
// The result for one call is a byte-identical prefix of the result for the
// next: history is append-only, a pin's message is frozen, and a new pin
// lands at the current end. The Codex input-suffix projection requires that
// (docs/design/codex-websocket-chaining.md).
//
// Only compaction shortens history; a pin past the new end is clamped to it
// rather than dropped, since that turn has no chain left to preserve.
func replayAmbientPins(history []message.Message, pins []ambientPin) []message.Message {
	if len(pins) == 0 {
		return history
	}
	out := make([]message.Message, 0, len(history)+len(pins))
	pi := 0
	for i := 0; i <= len(history); i++ {
		for pi < len(pins) && min(pins[pi].at, len(history)) == i {
			out = append(out, pins[pi].msg)
			pi++
		}
		if i < len(history) {
			out = append(out, history[i])
		}
	}
	return out
}

func (s *Session) withPinnedAmbient(history []message.Message, segs []ambientSegment) []message.Message {
	for _, seg := range segs {
		s.pinAmbient(seg.kind, seg.text, len(history))
	}
	s.mu.Lock()
	pins := append([]ambientPin(nil), s.ambientPins...)
	s.mu.Unlock()
	return replayAmbientPins(history, pins)
}
