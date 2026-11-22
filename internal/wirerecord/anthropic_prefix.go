package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

const prefixModel = "claude-fable-5-2"

func lookupTool(description string) obj {
	return obj{
		"name": "lookup", "description": description,
		"input_schema": obj{"type": "object", "properties": obj{"key": obj{"type": "string"}}, "required": []string{"key"}},
	}
}

// anthropicBlocks rebuilds the content blocks of a streamed message, so a
// later request can replay them unchanged.
func anthropicBlocks(r reply) ([]obj, error) {
	var blocks []obj
	var input string
	for _, ev := range parseSSE(r.Body) {
		var f struct {
			ContentBlock obj `json:"content_block"`
			Delta        struct {
				Type, Text, Thinking, Signature string
				PartialJSON                     string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(ev.Data), &f) != nil {
			continue
		}
		switch ev.Name {
		case "content_block_start":
			blocks, input = append(blocks, f.ContentBlock), ""
		case "content_block_delta":
			cur := blocks[len(blocks)-1]
			switch f.Delta.Type {
			case "text_delta":
				cur["text"] = fmt.Sprint(cur["text"]) + f.Delta.Text
			case "thinking_delta":
				cur["thinking"] = fmt.Sprint(cur["thinking"]) + f.Delta.Thinking
			case "signature_delta":
				sig, _ := cur["signature"].(string)
				cur["signature"] = sig + f.Delta.Signature
			case "input_json_delta":
				input += f.Delta.PartialJSON
			}
		case "content_block_stop":
			cur := blocks[len(blocks)-1]
			if cur["type"] != "tool_use" || input == "" {
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(input), &v); err != nil {
				return nil, fmt.Errorf("tool input: %w", err)
			}
			cur["input"] = v
		}
	}
	return blocks, nil
}

func blockOfType(blocks []obj, kind string) (obj, bool) {
	for _, b := range blocks {
		if b["type"] == kind {
			return b, true
		}
	}
	return nil, false
}

// recordAnthropicPrefix replays one thinking block three times: behind the
// prefix that produced it, behind a first message that a summary replaced, and
// behind that summary with a changed system prompt and changed tools. It also
// sends the summary with the tool call of the turn and no thinking block.
func recordAnthropicPrefix(r *recorder, _ string) error {
	key, err := requireEnv("ANTHROPIC_API_KEY")
	if err != nil {
		return err
	}
	base := func(system string, tools, messages []obj) obj {
		return obj{"model": prefixModel, "max_tokens": 4096, "thinking": obj{"type": "adaptive"}, "system": system, "tools": tools, "messages": messages}
	}
	prompt := obj{"role": "user", "content": "Work out 17 times 23, then take the remainder after division by 7. Think it through, then call the lookup tool with that remainder as the key."}
	oldTools := []obj{lookupTool("Look up the value of a key.")}

	first, err := anthropicCall(key, base("You are a careful assistant.", oldTools, []obj{prompt}))
	if err != nil || okStream(first) != nil {
		return fmt.Errorf("first call: %v %v", err, okStream(first))
	}
	blocks, err := anthropicBlocks(first)
	if err != nil {
		return err
	}
	use, ok := blockOfType(blocks, "tool_use")
	if !ok {
		return fmt.Errorf("first call holds no tool_use block")
	}
	if _, ok := blockOfType(blocks, "thinking"); !ok {
		if _, ok := blockOfType(blocks, "redacted_thinking"); !ok {
			return fmt.Errorf("first call holds no thinking block")
		}
	}
	turn := []obj{
		{"role": "assistant", "content": blocks},
		{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": use["id"], "content": "The value is blue."}}},
	}

	same, err := anthropicCall(key, base("You are a careful assistant.", oldTools, append([]obj{prompt}, turn...)))
	if err != nil || okStream(same) != nil {
		return fmt.Errorf("baseline: %v %v", err, okStream(same))
	}
	if err := r.save("anthropic.prefix-baseline.sse", same.Body); err != nil {
		return err
	}

	newTools := []obj{lookupTool("Look up the value that a key holds in the store."), {
		"name": "other", "description": "Return the current status.",
		"input_schema": obj{"type": "object", "properties": obj{}},
	}}
	summary := obj{"role": "user", "content": "Summary of the conversation so far: the user asked for the remainder of 17 times 23 after division by 7, and for a lookup of that remainder as a key."}
	compacted, err := anthropicCall(key, base("You are a careful assistant.", oldTools, append([]obj{summary}, turn...)))
	if err != nil {
		return err
	}
	if err := r.saveReply("anthropic.prefix-compacted", compacted); err != nil {
		return err
	}
	var plain []obj
	for _, b := range blocks {
		if b["type"] != "thinking" && b["type"] != "redacted_thinking" {
			plain = append(plain, b)
		}
	}
	dropped, err := anthropicCall(key, base("You are a careful assistant.", oldTools, []obj{summary, {"role": "assistant", "content": plain}, turn[1]}))
	if err != nil {
		return err
	}
	if err := r.saveReply("anthropic.prefix-dropped", dropped); err != nil {
		return err
	}
	changed, err := anthropicCall(key, base("You are a careful assistant. Answer in one sentence.", newTools, append([]obj{summary}, turn...)))
	if err != nil {
		return err
	}
	return r.saveReply("anthropic.prefix-changed", changed)
}

// saveReply saves a 200 as a stream and any other status as an error fixture.
func (r *recorder) saveReply(name string, rep reply) error {
	if rep.Status == http.StatusOK {
		return r.save(name+".sse", rep.Body)
	}
	fix, err := errorFixture(rep)
	if err != nil {
		return err
	}
	return r.save(name+".error.json", fix)
}
