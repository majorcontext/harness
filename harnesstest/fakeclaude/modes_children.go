package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	delegateText = "delegate"
	childPrompt  = "child work"
	reportMarker = "[tasks:"
	reportText   = "A background task you started has finished"
	slowSuffix   = "slow"
	gateFile     = "child.gate"
	gateWait     = 20 * time.Second
	settleWait   = 30 * time.Second
)

// childModes run one scenario across several invocations, told apart by the
// first input line.
var childModes = map[string]mode{
	"child_report":              childParent(childPrompt),
	"child_reports":             childParent(childPrompt+" a", childPrompt+" b"),
	"child_report_after_prompt": childParent(childPrompt + " " + slowSuffix),
}

// childParent is the parent of children that end while the parent runs. The
// parent starts each child through the task tool of the harness MCP server,
// one after the other, waits until each has ended, then reads one more input
// line. A line in that window shows that harness delivered a report in the
// middle of the turn.
// A child answers with its prompt, so a report names it. A child that ends
// with the slow suffix waits for the gate file in its work dir, so a test
// that writes the file after it queued a prompt gets the prompt queued before
// the child ends. A turn that carries a report
// in its first line only acknowledges it, and so does any other prompt.
func childParent(prompts ...string) mode {
	return func(f *fake) {
		text, _ := queuedContent(f.first)
		switch {
		case strings.Contains(text, childPrompt):
			if strings.HasSuffix(text, slowSuffix) {
				awaitGate()
			}
			answer := strings.TrimSpace("child done " + strings.TrimSpace(strings.TrimPrefix(text, childPrompt)))
			f.emit(say(answer), success(answer, 1, 1))
		case strings.Contains(text, reportMarker), strings.Contains(text, reportText):
			f.emit(say("noted"), success("noted", 1, 1))
		case strings.Contains(text, delegateText):
			spawnChildren(f, prompts)
		default:
			f.emit(say("ok"), success("ok", 1, 1))
		}
	}
}

func awaitGate() {
	for deadline := time.Now().Add(gateWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(gateFile); err == nil {
			return
		}
	}
}

func spawnChildren(f *fake, prompts []string) {
	f.emit(say("Delegating."))
	srv, ok := hostedServer()
	if !ok {
		f.emit(result("error_during_execution", true, "no harness server in --mcp-config", 0, 0))
		return
	}
	for _, p := range prompts {
		id, err := spawnChild(srv, p)
		if err == nil {
			err = awaitEnded(srv, id)
		}
		if err != nil {
			f.emit(result("error_during_execution", true, err.Error(), 0, 0))
			return
		}
	}
	answer := "no second message received"
	if content, ok := awaitQueued(f); ok {
		answer = "received queued: " + content
	}
	f.emit(say(answer), success(answer, 1, 1))
}

func taskCall(srv hostedEntry, args obj) (map[string]any, error) {
	call := obj{"name": "task", "arguments": args, "_meta": obj{toolUseMeta: "toolu_task"}}
	body, err := hostedPost(srv, "tools/call", call)
	if err != nil {
		return nil, err
	}
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil || len(rpc.Result.Content) == 0 {
		return nil, fmt.Errorf("task response: %s", body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &out); err != nil {
		return nil, fmt.Errorf("task result: %s", rpc.Result.Content[0].Text)
	}
	return out, nil
}

func spawnChild(srv hostedEntry, prompt string) (string, error) {
	out, err := taskCall(srv, obj{"agent": "general-purpose", "prompt": prompt})
	if err != nil {
		return "", err
	}
	id, _ := out["session_id"].(string)
	if id == "" {
		return "", errors.New("task spawn returned no session_id")
	}
	return id, nil
}

// awaitEnded polls the status of a child until its last turn has ended, so
// the report of the child reaches the parent while the parent still waits.
func awaitEnded(srv hostedEntry, id string) error {
	for deadline := time.Now().Add(settleWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		out, err := taskCall(srv, obj{"action": "status", "session_id": id})
		if err != nil {
			return err
		}
		if s, _ := out["status"].(string); s != "" && s != "running" {
			return nil
		}
	}
	return fmt.Errorf("child %s did not end", id)
}
