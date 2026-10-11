package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/wirescrub"
)

// cliArgs are the flags that the claudecode backend passes for a run with one
// built-in tool.
func cliArgs(model, tools string, extra ...string) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--forward-subagent-text", "--thinking-display", "summarized",
		"--disallowedTools", "Agent,Workflow,ScheduleWakeup,CronCreate,CronDelete,CronList",
		"--tools", tools, "--strict-mcp-config", "--model", model, "--session-mirror",
		"--setting-sources", "project", "--disable-slash-commands"}
	return append(args, extra...)
}

// runCLI sends each prompt in turn and reads frames until the result of each.
// With interrupt set it sends SIGINT when a frame holds a tool call.
func runCLI(dir string, prompts []string, args []string, interrupt bool) ([]byte, error) {
	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
	cmd.Env = withoutKeys(os.Environ())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for _, prompt := range prompts {
			line, _ := json.Marshal(obj{"type": "user", "message": obj{"role": "user", "content": prompt}})
			if _, err := stdin.Write(append(line, '\n')); err != nil {
				done <- err
				return
			}
			for sc.Scan() {
				raw.Write(sc.Bytes())
				raw.WriteByte('\n')
				var f struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(sc.Bytes(), &f) == nil && f.Type == "result" {
					break
				}
				if interrupt && f.Type == "assistant" && bytes.Contains(sc.Bytes(), []byte(`"type":"tool_use"`)) {
					_ = cmd.Process.Signal(os.Interrupt)
				}
			}
		}
		done <- sc.Err()
	}()
	select {
	case err := <-done:
		_ = stdin.Close()
		_ = cmd.Wait()
		return raw.Bytes(), err
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("claude did not finish")
	}
}

func withoutKeys(env []string) []string {
	var out []string
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") || strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (r *recorder) saveJSONL(name string, raw []byte) error {
	short, err := wirescrub.TruncateJSONL(raw, 300)
	if err != nil {
		return err
	}
	return r.save(name, short)
}

func recordClaudeCode(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []string{"README.md", "main.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644); err != nil {
			return err
		}
	}
	tool, err := runCLI(dir, []string{"Run ls with the Bash tool in the current directory, then name the files you saw in one short sentence."},
		cliArgs(claudeModel, "Bash", "--effort", "medium"), false)
	if err != nil {
		return fmt.Errorf("tool run: %w", err)
	}
	if err := r.saveJSONL("claudecode.tool.jsonl", tool); err != nil {
		return err
	}
	stopped, err := runCLI(dir, []string{"Run sleep 30 with the Bash tool, then say done."}, cliArgs(claudeModel, "Bash"), true)
	if err != nil {
		return fmt.Errorf("interrupt run: %w", err)
	}
	if err := r.saveJSONL("claudecode.interrupt.jsonl", stopped); err != nil {
		return err
	}
	bad, err := runCLI(dir, []string{"hi"}, cliArgs("no-such-model", "Bash"), false)
	if err != nil {
		return fmt.Errorf("error run: %w", err)
	}
	if err := r.saveJSONL("claudecode.error.jsonl", bad); err != nil {
		return err
	}
	parked, resumed, err := runQuestion(dir)
	if err != nil {
		return err
	}
	if err := r.saveJSONL("claudecode.question.jsonl", parked); err != nil {
		return err
	}
	return r.saveJSONL("claudecode.question-resume.jsonl", resumed)
}

const (
	deferHook      = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"defer"}}`
	questionPrompt = "Use the AskUserQuestion tool once to ask which database to use, with the options PostgreSQL and SQLite. Do nothing else first."
)

func questionHook(command string) string {
	hook := obj{"matcher": "AskUserQuestion", "hooks": []obj{{"type": "command", "command": command}}}
	data, _ := json.Marshal(obj{"hooks": obj{"PreToolUse": []obj{hook}}})
	return string(data)
}

func questionArgs(settings string, extra ...string) []string {
	return cliArgs(claudeModel, "AskUserQuestion", append([]string{"--permission-prompt-tool", "stdio", "--settings", settings}, extra...)...)
}

// runQuestion records the two runs of an AskUserQuestion flow as the backend
// drives it: a run that the defer hook parks, and a resume that answers the
// parked call over the control channel.
func runQuestion(dir string) (parked, resumed []byte, err error) {
	parked, err = runCLI(dir, []string{questionPrompt}, questionArgs(questionHook("cat >/dev/null; printf '%s' '"+deferHook+"'")), false)
	if err != nil {
		return nil, nil, fmt.Errorf("park run: %w", err)
	}
	var sessionID, callID string
	var input json.RawMessage
	for _, line := range bytes.Split(parked, []byte("\n")) {
		var f struct {
			SessionID string `json:"session_id"`
			Message   struct {
				Content []struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		if f.SessionID != "" {
			sessionID = f.SessionID
		}
		for _, c := range f.Message.Content {
			if c.Type == "tool_use" && c.Name == "AskUserQuestion" {
				callID, input = c.ID, c.Input
			}
		}
	}
	if sessionID == "" || callID == "" {
		return nil, nil, fmt.Errorf("park run: no session or AskUserQuestion call in the output")
	}
	var updated map[string]any
	if err := json.Unmarshal(input, &updated); err != nil {
		return nil, nil, err
	}
	var asked struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(input, &asked); err != nil || len(asked.Questions) == 0 || len(asked.Questions[0].Options) == 0 {
		return nil, nil, fmt.Errorf("park run: the AskUserQuestion call holds no question with options")
	}
	updated["answers"] = obj{asked.Questions[0].Question: asked.Questions[0].Options[0].Label}
	pass := `input=$(cat); case "$input" in *'"` + callID + `"'*) ;; *) printf '%s' '` + deferHook + `';; esac`
	resumed, err = runResume(dir, questionArgs(questionHook(pass), "--resume", sessionID), obj{"behavior": "allow", "updatedInput": updated})
	if err != nil {
		return nil, nil, fmt.Errorf("resume run: %w", err)
	}
	return parked, resumed, nil
}

// runResume answers the first can_use_tool request with decision and reads
// frames until the result.
func runResume(dir string, args []string, decision obj) ([]byte, error) {
	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
	cmd.Env = withoutKeys(os.Environ())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			raw.Write(sc.Bytes())
			raw.WriteByte('\n')
			var f struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
			}
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			if f.Type == "control_request" {
				reply, _ := json.Marshal(obj{"type": "control_response", "response": obj{"subtype": "success", "request_id": f.RequestID, "response": decision}})
				if _, err := stdin.Write(append(reply, '\n')); err != nil {
					done <- err
					return
				}
			}
			if f.Type == "result" {
				break
			}
		}
		done <- sc.Err()
	}()
	select {
	case err := <-done:
		_ = stdin.Close()
		_ = cmd.Wait()
		return raw.Bytes(), err
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("claude did not finish")
	}
}

// recordClaudeCodePartial records a turn with thinking and text that the CLI
// streams block by block, as the claudecode backend runs it.
func recordClaudeCodePartial(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := runCLI(dir, []string{"Think it over, say in one short sentence what you will do, run echo hi with the Bash tool, then greet me in one short sentence."},
		cliArgs(claudeModel, "Bash", "--effort", "medium", "--include-partial-messages"), false)
	if err != nil {
		return fmt.Errorf("partial run: %w", err)
	}
	return r.saveJSONL("claudecode.partial.jsonl", raw)
}
