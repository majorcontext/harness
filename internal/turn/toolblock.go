package turn

import "github.com/majorcontext/harness/protocol"

// ToolBlockSink receives tool block lifecycle notifications.
type ToolBlockSink interface {
	ToolBlockStarted(c protocol.ToolCall)
	ToolBlockAvailable(c protocol.ToolCall)
}

type toolBlockTurn interface {
	ToolBlock(itemID string, c protocol.ToolCall, available bool)
}

func (w watched) ToolBlockStarted(c protocol.ToolCall) {
	w.alive()
	if s, ok := w.Sink.(ToolBlockSink); ok {
		s.ToolBlockStarted(c)
	}
}

func (w watched) ToolBlockAvailable(c protocol.ToolCall) {
	w.alive()
	if s, ok := w.Sink.(ToolBlockSink); ok {
		s.ToolBlockAvailable(c)
	}
}

func (s *sink) ToolBlockStarted(c protocol.ToolCall) {
	if c.ID == "" || c.Name == "" {
		return
	}
	if s.toolBlocks == nil {
		s.toolBlocks = make(map[string]bool)
	}
	if s.toolBlocks[c.ID] {
		return
	}
	s.toolBlocks[c.ID] = true
	if s.output != nil {
		*s.output = true
	}
	if s.item == "" {
		s.item = s.Started()
	}
	if out, ok := s.Turn.(toolBlockTurn); ok {
		out.ToolBlock(s.item, c, false)
	}
}

func (s *sink) ToolBlockAvailable(c protocol.ToolCall) {
	if c.ID == "" || c.Name == "" {
		return
	}
	s.ToolBlockStarted(c)
	if out, ok := s.Turn.(toolBlockTurn); ok {
		out.ToolBlock(s.item, c, true)
	}
	delete(s.toolBlocks, c.ID)
}
