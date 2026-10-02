package harnesstest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/klauspost/compress/zstd"
)

const frameTextDelta = "response.output_text.delta"

// optionalParams are the request params a provider entry can omit from the wire.
var optionalParams = []string{"max_output_tokens", "temperature", "top_p", "metadata"}

// openAIBody is a decoded Responses request body, from an HTTP POST or a
// websocket response.create frame.
type openAIBody struct {
	Type               string            `json:"type"`
	Model              string            `json:"model"`
	Instructions       string            `json:"instructions"`
	Input              []json.RawMessage `json:"input"`
	ServiceTier        string            `json:"service_tier"`
	PreviousResponseID string            `json:"previous_response_id"`
	Generate           *bool             `json:"generate"`
	Tools              []struct {
		Name string `json:"name"`
	} `json:"tools"`

	params []string // optional params present on the wire
}

func decodeOpenAIBody(raw []byte) (openAIBody, error) {
	var b openAIBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return b, err
	}
	for _, p := range optionalParams {
		if _, ok := keys[p]; ok {
			b.params = append(b.params, p)
		}
	}
	return b, nil
}

// decodeBodyEncoding undoes the request Content-Encoding. The Codex HTTP path
// sends zstd.
func decodeBodyEncoding(raw []byte, encoding string) ([]byte, error) {
	switch encoding {
	case "":
		return raw, nil
	case "zstd":
		dec, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		return dec.DecodeAll(raw, nil)
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", encoding)
	}
}

type wireItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Output    string `json:"output"`
	Content   []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// request adapts a Responses body to a Request. Consecutive items of one role
// become one Message, as Anthropic's alternating turns do.
func (o *OpenAI) request(b openAIBody) Request {
	req := Request{System: b.Instructions, Model: b.Model, ServiceTier: b.ServiceTier}
	for _, t := range b.Tools {
		req.Tools = append(req.Tools, t.Name)
	}
	sort.Strings(req.Tools)
	add := func(role string, p ...Part) {
		if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == role {
			req.Messages[n-1].Parts = append(req.Messages[n-1].Parts, p...)
			return
		}
		req.Messages = append(req.Messages, Message{Role: role, Parts: p})
	}
	o.wmu.Lock()
	defer o.wmu.Unlock()
	for _, raw := range b.Input {
		var it wireItem
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		switch it.Type {
		case "message":
			var parts []Part
			for _, c := range it.Content {
				parts = append(parts, Part{Kind: "text", Text: c.Text})
			}
			add(it.Role, parts...)
		case "function_call":
			var input map[string]any
			_ = json.Unmarshal([]byte(it.Arguments), &input)
			add("assistant", Part{Kind: "tool_use", ToolName: it.Name, ToolInput: input, ToolUseID: it.CallID})
		case "function_call_output":
			add("user", Part{Kind: "tool_result", Text: it.Output, ToolName: o.callNames[it.CallID], ToolUseID: it.CallID})
		}
	}
	return req
}

// frame is one Responses stream event. data carries the event type in "type".
type frame struct {
	name string
	data obj
}

func newFrame(name string, data obj) frame {
	data["type"] = name
	return frame{name: name, data: data}
}

// nextResponseID numbers responses by kind, so a prewarm does not shift the
// id of the first scripted response.
func (o *OpenAI) nextResponseID(prefix string) string {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.responses[prefix]++
	return prefix + strconv.Itoa(o.responses[prefix])
}

func completedFrame(id string, u Usage) frame {
	return newFrame("response.completed", obj{"response": obj{
		"id": id, "usage": obj{"input_tokens": u.Input, "output_tokens": u.Output, "input_tokens_details": obj{"cached_tokens": 0}},
	}})
}

func (o *OpenAI) prewarmFrames() (string, []frame) {
	id := o.nextResponseID("resp_warm_")
	return id, []frame{newFrame("response.created", obj{"response": obj{"id": id}}), completedFrame(id, Usage{})}
}

func previousResponseNotFound(id string) frame {
	return newFrame("error", obj{"status": http.StatusBadRequest, "error": obj{
		"type": "invalid_request_error", "code": "previous_response_not_found",
		"message": fmt.Sprintf("Previous response with id '%s' not found.", id),
	}})
}

// replyFrames builds the events for step's Reply and returns the response id.
// A websocket response carries its rate limits as a frame, an SSE response as
// headers, so ws selects the frame.
func (o *OpenAI) replyFrames(step Step, ws bool) (string, []frame) {
	rep := step.Reply
	usage := rep.Usage
	if usage == (Usage{}) {
		usage = Usage{Input: 5, Output: 3}
	}
	id := o.nextResponseID("resp_")
	frames := []frame{newFrame("response.created", obj{"response": obj{"id": id}})}
	if rl := o.opts.Replies[step.Name].RateLimits; ws && rl != nil {
		frames = append(frames, rl.frame())
	}
	idx := 0
	if rep.Text != "" || len(rep.ToolCalls) == 0 {
		msg := obj{"type": "message", "role": "assistant", "content": []obj{{"type": "output_text", "text": rep.Text}}}
		frames = append(frames,
			newFrame("response.output_item.added", obj{"output_index": idx, "item": obj{"type": "message", "role": "assistant", "content": []obj{}}}),
			newFrame(frameTextDelta, obj{"output_index": idx, "delta": rep.Text}),
			newFrame("response.output_item.done", obj{"output_index": idx, "item": msg}),
		)
		idx++
	}
	for _, tc := range rep.ToolCalls {
		args, _ := json.Marshal(orEmpty(tc.Input))
		o.wmu.Lock()
		o.callNames[tc.ID] = tc.Name
		o.wmu.Unlock()
		item := obj{"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": string(args)}
		frames = append(frames,
			newFrame("response.output_item.added", obj{"output_index": idx, "item": obj{"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": ""}}),
			newFrame("response.output_item.done", obj{"output_index": idx, "item": item}),
		)
		idx++
	}
	return id, append(frames, completedFrame(id, usage))
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// frame builds the codex.rate_limits websocket event.
func (rl *RateLimits) frame() frame {
	window := func(w *RateWindow) any {
		if w == nil {
			return nil
		}
		return obj{"used_percent": w.UsedPercent, "window_minutes": w.WindowMinutes, "reset_at": w.ResetAt}
	}
	return newFrame("codex.rate_limits", obj{
		"plan_type":   rl.Plan,
		"rate_limits": obj{"primary": window(rl.Primary), "secondary": window(rl.Secondary)},
	})
}

// setHeaders writes the x-codex-* headers of an HTTP response.
func (rl *RateLimits) setHeaders(h http.Header) {
	if rl.Plan != "" {
		h.Set("x-codex-plan-type", rl.Plan)
	}
	for _, w := range []struct {
		prefix string
		window *RateWindow
	}{{"primary", rl.Primary}, {"secondary", rl.Secondary}, {"bengalfox-primary", rl.BengalfoxPrimary}} {
		if w.window == nil {
			continue
		}
		h.Set("x-codex-"+w.prefix+"-used-percent", strconv.FormatFloat(w.window.UsedPercent, 'f', -1, 64))
		h.Set("x-codex-"+w.prefix+"-window-minutes", strconv.FormatInt(w.window.WindowMinutes, 10))
		h.Set("x-codex-"+w.prefix+"-reset-at", strconv.FormatInt(w.window.ResetAt, 10))
	}
}
