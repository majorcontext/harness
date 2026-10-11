package claudecode

import (
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// liveMessage is one API response that stream events are streaming.
type liveMessage struct {
	id     string
	blocks []*liveBlock
	// byIndex maps the index of a content block to its entry in blocks.
	byIndex map[int]*liveBlock
}

// liveBlock is a text or thinking block, and the text of it that the stream
// events already sent.
type liveBlock struct {
	typ      string
	text     strings.Builder
	recorded bool
}

// streamEvent forwards the text and thinking deltas of a stream_event frame
// as they arrive. The assistant frame of the block follows and records it.
// A tool call has no delta: it shows when its assistant frame arrives.
func (r *run) streamEvent(env envelope) error {
	ev := env.Event
	if ev == nil {
		return nil
	}
	parent := env.ParentToolUseID
	switch ev.Type {
	case "message_start":
		if ev.Message == nil || ev.Message.ID == "" {
			delete(r.live, parent)
			return nil
		}
		if r.pending != nil && r.pendingID != ev.Message.ID && r.pending.ParentCallID == parent {
			if err := r.flush(); err != nil {
				return err
			}
		}
		if old := r.live[parent]; old != nil && old.streamed() {
			r.thread(parent).Drop()
		}
		if r.live == nil {
			r.live = map[string]*liveMessage{}
		}
		r.live[parent] = &liveMessage{id: ev.Message.ID, byIndex: map[int]*liveBlock{}}
	case "content_block_start":
		lm := r.live[parent]
		if lm == nil || ev.ContentBlock == nil {
			return nil
		}
		typ := ""
		switch ev.ContentBlock.Type {
		case "text":
			typ = eventlog.PartText
		case "thinking":
			typ = eventlog.PartReasoning
		default:
			return nil
		}
		b := &liveBlock{typ: typ}
		lm.blocks = append(lm.blocks, b)
		lm.byIndex[ev.Index] = b
	case "content_block_delta":
		lm := r.live[parent]
		if lm == nil || ev.Delta == nil {
			return nil
		}
		b := lm.byIndex[ev.Index]
		if b == nil {
			return nil
		}
		text := ""
		switch {
		case b.typ == eventlog.PartText && ev.Delta.Type == "text_delta":
			text = ev.Delta.Text
		case b.typ == eventlog.PartReasoning && ev.Delta.Type == "thinking_delta":
			text = ev.Delta.Thinking
		}
		if text == "" {
			return nil
		}
		b.text.WriteString(text)
		r.thread(parent).Delta(lm.id, turn.Delta{Type: b.typ, Text: text})
	}
	return nil
}

// unstreamed returns the part of text part p that the stream events did not
// send, which is all of it when the CLI runs without partial messages. The
// assistant frame of a block follows its deltas, so a block that streamed
// completely sends nothing again.
func (r *run) unstreamed(parent, id string, p eventlog.Part) string {
	lm := r.live[parent]
	if lm == nil || lm.id != id {
		return p.Text
	}
	for _, b := range lm.blocks {
		if b.recorded || b.typ != p.Type || b.text.Len() == 0 {
			continue
		}
		b.recorded = true
		rest, ok := strings.CutPrefix(p.Text, b.text.String())
		if !ok {
			return ""
		}
		return rest
	}
	return p.Text
}

// streamed reports a block that streamed text and that no assistant frame
// recorded: the CLI dropped a reply that it began, such as a retried call.
func (m *liveMessage) streamed() bool {
	return slices.ContainsFunc(m.blocks, func(b *liveBlock) bool { return !b.recorded && b.text.Len() > 0 })
}

// replay sends the text that the thread has streamed for a block that no
// assistant frame recorded yet, as the first delta of a new live item. The
// item that just completed ends without that text, and the block joins the
// item that its assistant frame starts.
func (r *run) replay(parent string) {
	lm := r.live[parent]
	if lm == nil || r.stopped {
		return
	}
	for _, b := range lm.blocks {
		if !b.recorded && b.text.Len() > 0 {
			r.thread(parent).Delta(lm.id, turn.Delta{Type: b.typ, Text: b.text.String()})
		}
	}
}

// flatThread is the thread of a sink that keeps one live item.
type flatThread struct{ turn.Sink }

func (flatThread) Drop() {}

// thread returns the live item of the thread that parent names, "" being the
// main thread.
func (r *run) thread(parent string) turn.Thread {
	if t, ok := r.out.(turn.Threaded); ok {
		return t.Thread(parent)
	}
	return flatThread{r.out}
}
