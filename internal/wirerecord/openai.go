package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	chatURL      = "https://api.openai.com/v1/chat/completions"
	responsesURL = "https://api.openai.com/v1/responses"
)

func openaiCall(url, key string, body obj) (reply, error) {
	body["stream"] = true
	return post(url, map[string]string{"Authorization": "Bearer " + key}, body)
}

func recordChat(r *recorder, _ string) error {
	key, err := requireEnv("OPENAI_API_KEY")
	if err != nil {
		return err
	}
	tool := obj{"type": "function", "function": obj{
		"name": "list_files", "description": "List the files in a directory.",
		"parameters": obj{"type": "object", "properties": obj{"dir": obj{"type": "string"}}, "required": []string{"dir"}},
	}}
	opts := obj{"include_usage": true}
	call := func(body obj) (reply, error) {
		body["model"], body["stream_options"] = chatModel, opts
		rep, err := openaiCall(chatURL, key, body)
		if err == nil {
			err = okStream(rep)
		}
		return rep, err
	}
	text, err := call(obj{"max_tokens": 32, "messages": []obj{{"role": "user", "content": "Reply with the single word: ok"}}})
	if err != nil {
		return fmt.Errorf("text: %w", err)
	}
	if err := r.save("chat.text.sse", text.Body); err != nil {
		return err
	}
	prompt := obj{"role": "user", "content": "Use the list_files tool on the directory . and tell me what it returns."}
	first, err := call(obj{"max_tokens": 256, "tools": []obj{tool}, "messages": []obj{prompt}})
	if err != nil {
		return fmt.Errorf("tool: %w", err)
	}
	if err := r.save("chat.tool.sse", first.Body); err != nil {
		return err
	}
	id, name, args := "", "", ""
	for _, ev := range parseSSE(first.Body) {
		var c struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string                           `json:"id"`
						Function struct{ Name, Arguments string } `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(ev.Data), &c) != nil {
			continue
		}
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.ID != "" {
					id, name = tc.ID, tc.Function.Name
				}
				args += tc.Function.Arguments
			}
		}
	}
	if id == "" {
		return fmt.Errorf("no tool call in the stream")
	}
	messages := []obj{prompt,
		{"role": "assistant", "tool_calls": []obj{{"id": id, "type": "function", "function": obj{"name": name, "arguments": args}}}},
		{"role": "tool", "tool_call_id": id, "content": "README.md\nmain.go"},
	}
	second, err := call(obj{"max_tokens": 256, "tools": []obj{tool}, "messages": messages})
	if err != nil {
		return fmt.Errorf("tool result: %w", err)
	}
	if err := r.save("chat.tool-result.sse", second.Body); err != nil {
		return err
	}
	bad, err := openaiCall(chatURL, key, obj{"model": "no-such-model", "messages": []obj{{"role": "user", "content": "hi"}}})
	if err != nil {
		return err
	}
	fix, err := errorFixture(bad)
	if err != nil {
		return err
	}
	return r.save("chat.error.json", fix)
}

type responsesCaller struct{ key string }

func (c responsesCaller) call(body obj) (reply, error) {
	body["model"], body["store"] = responsesModel, false
	body["reasoning"] = obj{"effort": "low", "summary": "auto"}
	body["include"] = []string{"reasoning.encrypted_content"}
	rep, err := openaiCall(responsesURL, c.key, body)
	if err == nil {
		err = okStream(rep)
	}
	return rep, err
}

func userMessage(text string) obj {
	return obj{"type": "message", "role": "user", "content": []obj{{"type": "input_text", "text": text}}}
}

var responsesTool = obj{"type": "function", "name": "list_files", "description": "List the files in a directory.", "strict": false,
	"parameters": obj{"type": "object", "properties": obj{"dir": obj{"type": "string"}}, "required": []string{"dir"}}}

func recordResponses(r *recorder, _ string) error {
	key, err := requireEnv("OPENAI_API_KEY")
	if err != nil {
		return err
	}
	c := responsesCaller{key}
	text, err := c.call(obj{"max_output_tokens": 600, "input": []obj{userMessage("What is 17 times 23? Answer in one short sentence.")}})
	if err != nil {
		return fmt.Errorf("text: %w", err)
	}
	if !strings.Contains(string(text.Body), "response.reasoning_summary_text.delta") {
		return fmt.Errorf("the text stream holds no reasoning summary")
	}
	if err := r.save("responses.text.sse", text.Body); err != nil {
		return err
	}
	cut, err := c.call(obj{"max_output_tokens": 16, "input": []obj{userMessage("Explain in detail how a bicycle works.")}})
	if err != nil {
		return fmt.Errorf("incomplete: %w", err)
	}
	if !strings.Contains(string(cut.Body), "response.incomplete") {
		return fmt.Errorf("the capped response did not end as incomplete")
	}
	if err := r.save("responses.incomplete.sse", cut.Body); err != nil {
		return err
	}
	if err := recordResponsesTool(r, c); err != nil {
		return err
	}
	bad, err := openaiCall(responsesURL, key, obj{"model": "no-such-model", "input": "hi"})
	if err != nil {
		return err
	}
	fix, err := errorFixture(bad)
	if err != nil {
		return err
	}
	return r.save("responses.error.json", fix)
}

func recordResponsesTool(r *recorder, c responsesCaller) error {
	prompt := userMessage("Use the list_files tool on the directory . and tell me what it returns.")
	first, err := c.call(obj{"max_output_tokens": 800, "tools": []obj{responsesTool}, "input": []obj{prompt}})
	if err != nil {
		return fmt.Errorf("tool: %w", err)
	}
	if err := r.save("responses.tool.sse", first.Body); err != nil {
		return err
	}
	var output []json.RawMessage
	for _, ev := range parseSSE(first.Body) {
		if ev.Name != "response.completed" {
			continue
		}
		var done struct {
			Response struct {
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &done); err != nil {
			return err
		}
		output = done.Response.Output
	}
	callID := ""
	input := []any{prompt}
	for _, raw := range output {
		input = append(input, raw)
		var it struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(raw, &it) == nil && it.Type == "function_call" {
			callID = it.CallID
		}
	}
	if callID == "" {
		return fmt.Errorf("no function_call in the completed response")
	}
	input = append(input, obj{"type": "function_call_output", "call_id": callID, "output": "README.md\nmain.go"})
	second, err := c.call(obj{"max_output_tokens": 800, "tools": []obj{responsesTool}, "input": input})
	if err != nil {
		return fmt.Errorf("tool result: %w", err)
	}
	return r.save("responses.tool-result.sse", second.Body)
}
