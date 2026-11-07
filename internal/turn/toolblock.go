package turn

import (
	"bytes"
	"github.com/majorcontext/harness/protocol"
)

// ToolBlockSink receives tool block lifecycle notifications.
type ToolBlockSink interface {
	ToolBlockStarted(c protocol.ToolCall)
	ToolBlockAvailable(c protocol.ToolCall)
}

type toolBlockTurn interface {
	ToolBlock(itemID string, c protocol.ToolCall, available bool, failure error)
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
	if s.normalizeCtx != nil && s.normalizeCtx.Err() != nil {
		return
	}
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
		out.ToolBlock(s.item, c, false, nil)
	}
}

func (s *sink) ToolBlockAvailable(c protocol.ToolCall) {
	if s.normalizeCtx != nil && s.normalizeCtx.Err() != nil {
		return
	}
	if c.ID == "" || c.Name == "" {
		return
	}
	s.ToolBlockStarted(c)
	n := s.normalize(c)
	c = n.call
	if s.normalizeCtx != nil && s.normalizeCtx.Err() != nil {
		return
	}
	if out, ok := s.Turn.(toolBlockTurn); ok {
		out.ToolBlock(s.item, c, true, n.err)
	}
	delete(s.toolBlocks, c.ID)
}

type normalizedCall struct {
	call    protocol.ToolCall
	changed bool
	err     error
}

func (s *sink) normalize(c protocol.ToolCall) normalizedCall {
	if s.early == nil {
		return normalizedCall{call: c}
	}
	if n, ok := s.normalized[c.ID]; ok && c.ID != "" {
		return n
	}
	ctx := s.normalizeCtx
	if ctx == nil {
		ctx = s.early.ctx
	}
	out, err := s.early.t.normalize(ctx, c)
	n := normalizedCall{call: out, changed: err == nil && !bytes.Equal(c.Arguments, out.Arguments), err: err}
	if ctx.Err() != nil {
		return n
	}
	if s.normalized == nil {
		s.normalized = make(map[string]normalizedCall)
	}
	if c.ID != "" {
		s.normalized[c.ID] = n
		if err != nil {
			s.early.failures[c.ID] = err
		}
	} else {
		s.early.emptyFailures = append(s.early.emptyFailures, err)
	}
	return n
}
