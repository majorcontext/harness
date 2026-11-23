package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const toolChangeBeta = "mid-conversation-tool-changes-2026-07-01"

func toolRef(kind, name string) obj {
	return obj{"type": kind, "tool": obj{"type": "tool_reference", "name": name}}
}

// cacheRead returns the cache_read_input_tokens and cache_creation_input_tokens of a streamed message.
func cacheRead(r reply) (read, created int) {
	for _, ev := range parseSSE(r.Body) {
		if ev.Name != "message_start" {
			continue
		}
		var f struct {
			Message struct {
				Usage struct {
					Read    int `json:"cache_read_input_tokens"`
					Created int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(ev.Data), &f) == nil {
			return f.Message.Usage.Read, f.Message.Usage.Created
		}
	}
	return 0, 0
}

// marked returns messages with a cache breakpoint on the last block of the last message that is not a system message.
func marked(messages []obj) []obj {
	out := append([]obj(nil), messages...)
	at := len(out) - 1
	for at > 0 && out[at]["role"] == "system" {
		at--
	}
	last := out[at]
	blocks, ok := last["content"].([]obj)
	if !ok {
		blocks = []obj{{"type": "text", "text": last["content"]}}
	}
	blocks = append([]obj(nil), blocks...)
	end := obj{}
	for k, v := range blocks[len(blocks)-1] {
		end[k] = v
	}
	end["cache_control"] = obj{"type": "ephemeral"}
	blocks[len(blocks)-1] = end
	out[at] = obj{"role": last["role"], "content": blocks}
	return out
}

// recordAnthropicToolChange records one conversation that hides and shows tools with system messages: a request
// whose hidden tools carry defer_loading and no tool-search tool, a switch that removes one tool and adds two by
// reference after a tool result, and the switch back. Each request replays the thinking blocks before it.
func recordAnthropicToolChange(r *recorder, _ string) error {
	key, err := requireEnv("ANTHROPIC_API_KEY")
	if err != nil {
		return err
	}
	schema := obj{"type": "object", "properties": obj{"key": obj{"type": "string"}}, "required": []string{"key"}}
	tools := []obj{
		{"name": "code", "description": "Run a script. Input: the script text as key.", "input_schema": schema},
		{"name": "lookup", "description": "Look up the value of a key.", "input_schema": schema, "defer_loading": true},
		{"name": "other", "description": "Return the status of a key.", "input_schema": schema, "defer_loading": true},
	}
	system := []obj{{"type": "text", "cache_control": obj{"type": "ephemeral"},
		"text": "You are a careful assistant. Follow each instruction exactly.\n" + strings.Repeat("Reference line: the store holds keys and values, and each tool reads one key.\n", 600)}}
	call := func(messages []obj) (reply, error) {
		body := obj{"model": prefixModel, "max_tokens": 4096, "thinking": obj{"type": "adaptive"}, "system": system, "tools": tools, "messages": marked(messages), "stream": true}
		return post(anthropicURL, map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01", "anthropic-beta": toolChangeBeta}, body)
	}
	messages := []obj{{"role": "user", "content": "Work out 17 times 23 modulo 7. Think it through, then call the code tool with that number as the key. After each tool result, call one tool that you can see with the key \"next\", and prefer a tool that you did not call before."}}
	steps := []struct {
		name   string
		change []obj
	}{
		{"anthropic.toolchange-deferred", nil},
		{"anthropic.toolchange-switch", []obj{toolRef("tool_removal", "code"), toolRef("tool_addition", "lookup"), toolRef("tool_addition", "other")}},
		{"anthropic.toolchange-back", []obj{toolRef("tool_addition", "code"), toolRef("tool_removal", "lookup"), toolRef("tool_removal", "other")}},
	}
	for _, step := range steps {
		if step.change != nil {
			messages = append(messages, obj{"role": "system", "content": step.change})
		}
		rep, err := call(messages)
		if err != nil {
			return err
		}
		if err := r.saveReply(step.name, rep); err != nil {
			return err
		}
		if okStream(rep) != nil {
			fmt.Fprintf(os.Stderr, "%s: status %d\n", step.name, rep.Status)
			return nil
		}
		read, created := cacheRead(rep)
		blocks, err := anthropicBlocks(rep)
		if err != nil {
			return err
		}
		use, called := blockOfType(blocks, "tool_use")
		_, thought := blockOfType(blocks, "thinking")
		fmt.Fprintf(os.Stderr, "%s: status 200 cache_read %d cache_creation %d tool %v thinking %v\n", step.name, read, created, use["name"], thought)
		messages = append(messages, obj{"role": "assistant", "content": blocks})
		if called {
			messages = append(messages, obj{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": use["id"], "content": "The value is blue."}}})
		} else {
			messages = append(messages, obj{"role": "user", "content": "Continue: call one tool that you can see with the key \"next\"."})
		}
	}
	return nil
}
