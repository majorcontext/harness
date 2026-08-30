// Command fakeclaude is a stand-in for the real `claude` CLI, built once by
// engine's TestMain and pointed at via ClaudeCodeConfig.BinaryPath for
// engine/claude_code_backend_test.go. It emits a canned `--output-format
// stream-json` sequence (system/init, assistant text, a tool_use+
// tool_result pair, a final result) selected by the FAKE_CLAUDE_MODE
// environment variable, so the driver's event-mapping, resume, and usage
// logic can be tested against a REAL child process and REAL pipes without
// depending on an actual Anthropic subscription or the real binary.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	if logPath := os.Getenv("FAKE_CLAUDE_LOG"); logPath != "" {
		// Record this invocation's full argv for the test to inspect
		// afterward (the resume test's only way to see whether --resume
		// was passed on the SECOND call).
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			enc := json.NewEncoder(f)
			_ = enc.Encode(os.Args[1:])
			f.Close()
		}
	}

	// Drain (don't require) exactly one stdin line — the real driver
	// writes one turn message and closes stdin; reading it keeps this
	// stand-in honest about the protocol without validating its content.
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			// discard
		}
	}()

	sessionID := os.Getenv("FAKE_CLAUDE_SESSION_ID")
	if sessionID == "" {
		sessionID = "fake-session-1"
	}

	out := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(out, string(b))
		out.Flush()
	}

	emit(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
	})

	switch os.Getenv("FAKE_CLAUDE_MODE") {
	case "hang":
		// No signal handlers installed: an unhandled SIGINT/SIGTERM uses
		// Go's own default disposition, which terminates the process —
		// exactly the behavior the driver's abort/interrupt cascade
		// depends on (see claude_code_backend.go's signal-cascade
		// goroutine). Blocks far longer than any test's own timeout so a
		// working cascade is what ends this process, not a wall-clock
		// race.
		time.Sleep(time.Hour)
		return
	case "error":
		emit(map[string]any{
			"type":     "result",
			"subtype":  "error_during_execution",
			"is_error": true,
			"result":   "fake failure",
			"usage": map[string]any{
				"input_tokens":  11,
				"output_tokens": 3,
			},
		})
		return
	}

	// Default ("normal"): assistant text, a tool_use/tool_result pair,
	// then a final assistant text and result — one of each event shape
	// engine/claude_code_backend.go documents mapping.
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "Let me check that."},
			},
		},
	})
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "echo hi"}},
			},
		},
	})
	emit(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "hi\n"},
			},
		},
	})
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "Done — it printed hi."},
			},
		},
	})
	emit(map[string]any{
		"type":     "result",
		"subtype":  "success",
		"is_error": false,
		"result":   "Done — it printed hi.",
		"usage": map[string]any{
			"input_tokens":                101,
			"output_tokens":               42,
			"cache_read_input_tokens":     7,
			"cache_creation_input_tokens": 5,
		},
		"total_cost_usd": 0.0123,
	})
}
