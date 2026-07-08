package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/andybons/harness/message"
	"github.com/andybons/harness/provider"
)

// maxToolOutput caps captured output; the tail is kept since errors usually
// print last.
const maxToolOutput = 100 * 1024

func bashTool(timeout time.Duration) Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "bash",
			Description: "Execute a shell command and return its combined stdout and stderr. The command runs with `sh -c` in the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "The shell command to execute"}
				},
				"required": ["command"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Command == "" {
				return nil, fmt.Errorf("bash: missing command argument")
			}

			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := exec.CommandContext(cctx, "sh", "-c", in.Command)
			cmd.Dir = s.cfg.WorkDir
			cmd.Env = os.Environ()
			for k, v := range s.shellEnv(ctx, "bash", in.Command) {
				cmd.Env = append(cmd.Env, k+"="+v)
			}

			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()

			out := buf.Bytes()
			if len(out) > maxToolOutput {
				out = append([]byte("[output truncated]\n"), out[len(out)-maxToolOutput:]...)
			}
			text := string(out)

			if cctx.Err() == context.DeadlineExceeded {
				return message.Parts{&message.Text{Text: text + fmt.Sprintf("\n[command timed out after %s]", timeout)}}, fmt.Errorf("bash: timeout after %s", timeout)
			}
			if err != nil {
				if text == "" {
					text = err.Error()
				} else {
					text += "\n" + err.Error()
				}
				return nil, fmt.Errorf("%s", text)
			}
			return message.Parts{&message.Text{Text: text}}, nil
		},
	}
}
