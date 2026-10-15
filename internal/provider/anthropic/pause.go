package anthropic

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"time"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/provider"
)

// maxResumes bounds the legs that follow the first one in a model call. The API
// ends a call with the stop reason pause_turn when a server tool loop runs long.
const maxResumes = 3

// pauseReason is the IncompleteReason of a call that still paused after the last resume.
const pauseReason = "pause_turn"

// all lists the blocks of every leg, in order.
func (s *stream) all() []*assembledBlock {
	return append(append([]*assembledBlock(nil), s.prior...), s.blocks...)
}

// beginLeg starts the message of a leg. The message ID of the first leg names the whole call.
func (s *stream) beginLeg(id string) {
	if s.firstID == "" {
		s.firstID = id
		s.msgCreatedAt = time.Now().UTC()
	}
	s.msgID = s.firstID
}

// stop ends the leg that message_stop closes. A leg that paused sends its content back and reads the next leg;
// after maxResumes resumes the call ends incomplete. Any other leg ends the call.
func (s *stream) stop() error {
	if s.msgCreatedAt.IsZero() {
		s.msgCreatedAt = time.Now().UTC()
	}
	if !s.paused {
		s.finish(s.stopReason, "")
		return nil
	}
	if s.legs >= maxResumes {
		s.finish(provider.StopPaused, pauseReason)
		return nil
	}
	s.prior, s.blocks = s.all(), nil
	s.prompt = cmp.Or(s.prompt, s.usage.InputTokens+s.usage.CacheWriteTokens+s.usage.CacheReadTokens)
	s.carry = addUsage(s.carry, s.usage)
	s.usage, s.paused = provider.Usage{}, false
	s.legs++
	_ = s.body.Close()
	resp, err := s.resume(apiBlocksOf(s.prior))
	if err != nil && s.ctx.Err() != nil {
		return err
	}
	if err != nil {
		// A new call would run and bill the searches again, so a failed resume ends the call as the last pause does.
		s.finish(provider.StopPaused, pauseReason)
		return nil
	}
	s.body, s.r = resp.Body, bufio.NewReader(resp.Body)
	return nil
}

// finish queues the end of the call: the message of every leg, and the usage of all of them.
func (s *stream) finish(stop provider.StopReason, incomplete string) {
	s.blocks = s.all()
	s.prior = nil
	msg := s.assemble()
	if stop == provider.StopPaused {
		msg.Parts = slices.DeleteFunc(msg.Parts, unfinishedSearch)
	}
	s.queue = append(s.queue, provider.Event{
		Type:             provider.EventDone,
		Message:          msg,
		StopReason:       stop,
		IncompleteReason: incomplete,
		Usage:            addUsage(s.carry, s.usage),
		Prompt:           cmp.Or(s.prompt, s.usage.InputTokens+s.usage.CacheWriteTokens+s.usage.CacheReadTokens),
	})
	s.done = true
}

// unfinishedSearch reports a search part that has neither a result nor an error. A server call with no result is not replayed.
func unfinishedSearch(p message.Part) bool {
	w, ok := p.(*message.WebSearch)
	return ok && w.Error == "" && len(w.ProviderData) == 0 && len(w.Results) == 0
}

func addUsage(a, b provider.Usage) provider.Usage {
	return provider.Usage{InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens,
		CacheReadTokens: a.CacheReadTokens + b.CacheReadTokens, CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		WebSearchRequests: a.WebSearchRequests + b.WebSearchRequests}
}

// apiBlocksOf renders the blocks of the paused legs as the API sent them, for the request that resumes the call.
// An empty text block is left out, because the API refuses it. A block of a kind that the stream does not keep is left out.
func apiBlocksOf(blocks []*assembledBlock) []apiBlock {
	var out []apiBlock
	for _, b := range blocks {
		switch b.kind {
		case "text":
			if b.text.Len() > 0 {
				out = append(out, apiBlock{Type: "text", Text: b.text.String()})
			}
		case "thinking":
			thinking := b.text.String()
			out = append(out, apiBlock{Type: "thinking", Thinking: &thinking, Signature: b.signature})
		case "redacted_thinking":
			out = append(out, apiBlock{Type: "redacted_thinking", Data: b.redacted})
		case "tool_use", "server_tool_use":
			input := json.RawMessage(bytes.Clone(b.inputJSON.Bytes()))
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, apiBlock{Type: b.kind, ID: b.toolID, Name: b.toolName, Input: input})
		case "web_search_tool_result":
			out = append(out, apiBlock{Type: b.kind, ToolUseID: b.toolID, ResultContent: b.result})
		}
	}
	return out
}
