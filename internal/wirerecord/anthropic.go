package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const anthropicURL = "https://api.anthropic.com/v1/messages"

var listFilesTool = obj{
	"name": "list_files", "description": "List the files in a directory.",
	"input_schema": obj{"type": "object", "properties": obj{"dir": obj{"type": "string"}}, "required": []string{"dir"}},
}

func anthropicCall(key string, body obj) (reply, error) {
	body["stream"] = true
	return post(anthropicURL, map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}, body)
}

type toolUse struct{ ID, Name, Input string }

func anthropicToolUse(r reply) (toolUse, error) {
	var tu toolUse
	for _, ev := range parseSSE(r.Body) {
		var f struct {
			ContentBlock struct{ ID, Name, Type string } `json:"content_block"`
			Delta        struct {
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(ev.Data), &f) != nil {
			continue
		}
		if ev.Name == "content_block_start" && f.ContentBlock.Type == "tool_use" {
			tu.ID, tu.Name = f.ContentBlock.ID, f.ContentBlock.Name
		}
		tu.Input += f.Delta.PartialJSON
	}
	if tu.ID == "" {
		return tu, fmt.Errorf("no tool_use block in the stream")
	}
	if tu.Input == "" {
		tu.Input = "{}"
	}
	return tu, nil
}

func recordAnthropic(r *recorder, _ string) error {
	key, err := requireEnv("ANTHROPIC_API_KEY")
	if err != nil {
		return err
	}
	user := func(text string) obj { return obj{"role": "user", "content": text} }
	text, err := anthropicCall(key, obj{"model": anthropicModel, "max_tokens": 32, "messages": []obj{user("Reply with the single word: ok")}})
	if err != nil || okStream(text) != nil {
		return fmt.Errorf("text: %v %v", err, okStream(text))
	}
	if err := r.save("anthropic.text.sse", text.Body); err != nil {
		return err
	}

	prompt := user("Use the list_files tool on the directory . and tell me what it returns.")
	tool, err := anthropicCall(key, obj{"model": anthropicModel, "max_tokens": 256, "tools": []obj{listFilesTool}, "messages": []obj{prompt}})
	if err != nil || okStream(tool) != nil {
		return fmt.Errorf("tool: %v %v", err, okStream(tool))
	}
	if err := r.save("anthropic.tool.sse", tool.Body); err != nil {
		return err
	}
	tu, err := anthropicToolUse(tool)
	if err != nil {
		return err
	}
	var input any
	if err := json.Unmarshal([]byte(tu.Input), &input); err != nil {
		return fmt.Errorf("tool input: %w", err)
	}
	messages := []obj{prompt,
		{"role": "assistant", "content": []obj{{"type": "tool_use", "id": tu.ID, "name": tu.Name, "input": input}}},
		{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": tu.ID, "content": "README.md\nmain.go"}}},
	}
	result, err := anthropicCall(key, obj{"model": anthropicModel, "max_tokens": 256, "tools": []obj{listFilesTool}, "messages": messages})
	if err != nil || okStream(result) != nil {
		return fmt.Errorf("tool result: %v %v", err, okStream(result))
	}
	if err := r.save("anthropic.tool-result.sse", result.Body); err != nil {
		return err
	}

	thinking, err := anthropicCall(key, obj{"model": anthropicModel, "max_tokens": 2048, "thinking": obj{"type": "enabled", "budget_tokens": 1024}, "messages": []obj{user("What is 17 times 23? Answer in one short sentence.")}})
	if err != nil || okStream(thinking) != nil {
		return fmt.Errorf("thinking: %v %v", err, okStream(thinking))
	}
	if !strings.Contains(string(thinking.Body), `"thinking_delta"`) {
		return fmt.Errorf("thinking stream holds no thinking_delta")
	}
	if err := r.save("anthropic.thinking.sse", thinking.Body); err != nil {
		return err
	}

	bad, err := anthropicCall(key, obj{"model": "claude-no-such-model", "max_tokens": 16, "messages": []obj{user("hi")}})
	if err != nil {
		return err
	}
	fix, err := errorFixture(bad)
	if err != nil {
		return err
	}
	return r.save("anthropic.error.json", fix)
}
