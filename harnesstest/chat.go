package harnesstest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// NewChat starts a Server that speaks the OpenAI chat-completions wire at
// a path ending in "/chat/completions", as a gateway such as Bifrost does.
// Steps, matching, Block, Release, and error replies behave as for New. A
// request that a real gateway rejects fails the test: a wrong path, a body
// without stream and stream_options.include_usage set to true, an unknown
// role, malformed tool-call arguments, or a tool message with no matching
// assistant tool call.
func NewChat(t testing.TB, steps ...Step) *Server {
	t.Helper()
	return start(t, chatCodec, steps)
}

var chatCodec = codec{
	decode: decodeChatRequest, stream: (*Server).chatStream, writeError: writeChatError, replyError: writeChatReplyError,
	pathSuffix: "/chat/completions",
}

type chatWireRequest struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	User            string `json:"user"`
	PromptCacheKey  string `json:"prompt_cache_key"`
	Stream          bool   `json:"stream"`
	StreamOptions   struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []chatToolCall  `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func chatText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []chatContentPart
	_ = json.Unmarshal(raw, &parts)
	var texts []string
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// decodeChatRequest maps the chat wire onto Request. A run of "tool"
// messages becomes one user message of tool_result parts, the shape that
// LastToolResult reads.
func decodeChatRequest(body []byte, h http.Header) (Request, error) {
	var w chatWireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return Request{}, err
	}
	if !w.Stream {
		return Request{}, errors.New("stream is not true")
	}
	if !w.StreamOptions.IncludeUsage {
		return Request{}, errors.New("stream_options.include_usage is not true")
	}
	req := Request{
		Model: w.Model, ReasoningEffort: w.ReasoningEffort, User: w.User,
		PromptCacheKey: w.PromptCacheKey, Header: h.Clone(),
	}
	for _, t := range w.Tools {
		req.Tools = append(req.Tools, t.Function.Name)
	}
	sort.Strings(req.Tools)

	var system []string
	pending := map[string]string{}
	for _, m := range w.Messages {
		if m.Role != "tool" && len(pending) > 0 {
			return Request{}, fmt.Errorf("%s message follows assistant tool calls with %d unanswered", m.Role, len(pending))
		}
		switch m.Role {
		case "system":
			system = append(system, chatText(m.Content))
		case "user":
			req.Messages = append(req.Messages, Message{Role: "user", Parts: []Part{{Kind: "text", Text: chatText(m.Content)}}})
		case "assistant":
			msg := Message{Role: "assistant"}
			if text := chatText(m.Content); text != "" {
				msg.Parts = append(msg.Parts, Part{Kind: "text", Text: text})
			}
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = tc.Function.Name
				var input map[string]any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					return Request{}, fmt.Errorf("tool call %q has malformed arguments: %w", tc.ID, err)
				}
				msg.Parts = append(msg.Parts, Part{Kind: "tool_use", ToolName: tc.Function.Name, ToolInput: input, ToolUseID: tc.ID})
			}
			req.Messages = append(req.Messages, msg)
		case "tool":
			name, ok := pending[m.ToolCallID]
			if !ok {
				return Request{}, fmt.Errorf("tool message %q has no matching assistant tool call", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			part := Part{Kind: "tool_result", Text: chatText(m.Content), ToolName: name, ToolUseID: m.ToolCallID}
			if n := len(req.Messages); n > 0 && len(req.Messages[n-1].Parts) > 0 && req.Messages[n-1].Parts[0].Kind == "tool_result" {
				req.Messages[n-1].Parts = append(req.Messages[n-1].Parts, part)
			} else {
				req.Messages = append(req.Messages, Message{Role: "user", Parts: []Part{part}})
			}
		default:
			return Request{}, fmt.Errorf("unknown message role %q", m.Role)
		}
	}
	if len(pending) > 0 {
		return Request{}, fmt.Errorf("%d assistant tool calls have no tool message", len(pending))
	}
	req.System = strings.Join(system, "\n\n")
	return req, nil
}

var chatErrorTypes = map[int]string{
	http.StatusBadRequest:      "invalid_request_error",
	http.StatusUnauthorized:    "authentication_error",
	http.StatusNotFound:        "not_found_error",
	http.StatusTooManyRequests: "rate_limit_error",
}

func writeChatError(w http.ResponseWriter, status int, msg string) {
	writeChatErrorCode(w, status, msg, "")
}

func writeChatErrorCode(w http.ResponseWriter, status int, msg, code string) {
	typ, ok := chatErrorTypes[status]
	if !ok {
		typ = "server_error"
	}
	var wireCode any
	if code != "" {
		wireCode = code
	}
	body, _ := json.Marshal(obj{"error": obj{"message": msg, "type": typ, "code": wireCode}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeChatReplyError(w http.ResponseWriter, rep Reply) {
	if rep.RetryAfter != "" {
		w.Header().Set("Retry-After", rep.RetryAfter)
	}
	msg := rep.ErrorMessage
	if msg == "" {
		msg = "harnesstest: scripted error"
	}
	writeChatErrorCode(w, rep.HTTPStatus, msg, rep.ErrorCode)
}

var chatFinishReasons = map[string]string{"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length"}

func (s *Server) chatStream(w http.ResponseWriter, r *http.Request, n int, name string, rep Reply) {
	f, ok := w.(http.Flusher)
	if !ok {
		writeChatError(w, http.StatusInternalServerError, "no flusher")
		return
	}
	usage := rep.Usage
	if usage == (Usage{}) {
		usage = Usage{Input: 5, Output: 3}
	}
	stop := rep.StopReason
	if stop == "" {
		stop = "end_turn"
		if len(rep.ToolCalls) > 0 {
			stop = "tool_use"
		}
	}
	finish, ok := chatFinishReasons[stop]
	if !ok {
		finish = stop
	}
	id := fmt.Sprintf("chatcmpl-fake-%d", n)
	chunk := func(delta obj, finishReason any) {
		b, _ := json.Marshal(obj{"id": id, "object": "chat.completion.chunk", "choices": []obj{{"index": 0, "delta": delta, "finish_reason": finishReason}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		f.Flush()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	chunk(obj{"role": "assistant", "content": ""}, nil)

	if rep.Reasoning != "" {
		chunk(obj{"reasoning_content": rep.Reasoning}, nil)
	}
	gate := rep.Block && rep.Text == ""
	if rep.Text != "" || len(rep.ToolCalls) == 0 {
		chunk(obj{"content": rep.Text}, nil)
		if rep.Block && !s.block(r, name) {
			return
		}
	}
	for i, tc := range rep.ToolCalls {
		input := tc.Input
		if input == nil {
			input = map[string]any{}
		}
		args, _ := json.Marshal(input)
		chunk(obj{"tool_calls": []obj{{"index": i, "id": tc.ID, "type": "function", "function": obj{"name": tc.Name, "arguments": ""}}}}, nil)
		chunk(obj{"tool_calls": []obj{{"index": i, "function": obj{"arguments": string(args)}}}}, nil)
		if gate {
			gate = false
			if !s.block(r, name) {
				return
			}
		}
	}
	chunk(obj{}, finish)
	tail, _ := json.Marshal(obj{"id": id, "object": "chat.completion.chunk", "choices": []obj{}, "usage": obj{"prompt_tokens": usage.Input, "completion_tokens": usage.Output}})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", tail)
	f.Flush()
}
