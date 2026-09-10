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
	// cleared is pinned when text goes empty after a non-empty pin. Empty
	// for a kind whose absence says nothing (identity, one-shot notices):
	// history is append-only, so absence cannot be shown by omission.
	cleared string
}

type ambientPin struct {
	kind string
	// at is len(history) when this pin was first rendered, never less than
	// the previous pin's: replayAmbientPins inserts in one forward pass.
	// Only clampAmbientPins lowers it, when compaction shrinks history.
	at int
	// msg is frozen at pin time so replay is byte-identical by construction.
	msg  message.Message
	text string
}

// pinAmbient appends a pin when seg differs from the newest pin of its kind.
// Comparing against that pin keeps the non-idempotent task-notification
// segment safe: a retried or requeued turn re-renders the same text and adds
// no second pin.
func (s *Session) pinAmbient(seg ambientSegment, at int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last string
	for i := len(s.ambientPins) - 1; i >= 0; i-- {
		if s.ambientPins[i].kind == seg.kind {
			last = s.ambientPins[i].text
			break
		}
	}
	text := seg.text
	if text == "" {
		if last == "" || seg.cleared == "" {
			return
		}
		text = seg.cleared
	}
	if text == last {
		return
	}
	kind := seg.kind
	if n := len(s.ambientPins); n > 0 && s.ambientPins[n-1].at > at {
		at = s.ambientPins[n-1].at
	}
	s.ambientPins = append(s.ambientPins, ambientPin{
		kind: kind,
		at:   at,
		text: text,
		msg: message.Message{
			ID:        newID("msg"),
			Role:      message.RoleUser,
			Parts:     message.Parts{&message.EngineContext{Text: text}},
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
	s.clampAmbientPins(len(history))
	for _, seg := range segs {
		s.pinAmbient(seg, len(history))
	}
	s.mu.Lock()
	pins := append([]ambientPin(nil), s.ambientPins...)
	s.mu.Unlock()
	return replayAmbientPins(history, pins)
}

// clampAmbientPins lowers any slot past n permanently. Clamping only at
// replay would let a pin stranded by compaction float to whatever the end
// happens to be, moving it again on every later call.
func (s *Session) clampAmbientPins(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.ambientPins {
		if s.ambientPins[i].at > n {
			s.ambientPins[i].at = n
		}
	}
}
