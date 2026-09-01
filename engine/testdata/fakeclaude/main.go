// Command fakeclaude is a stand-in for the real `claude` CLI, built once by
// engine's TestMain and pointed at via ClaudeCodeConfig.BinaryPath for
// engine/claude_code_backend_test.go. It emits a canned `--output-format
// stream-json` sequence (system/init, assistant text, a tool_use+
// tool_result pair, a final result) selected by the FAKE_CLAUDE_MODE
// environment variable, so the driver's event-mapping, resume, and usage
// logic can be tested against a REAL child process and REAL pipes without
// depending on an actual Anthropic subscription or the real binary.
//
// Beyond "normal"/"hang"/"error", a handful of narrower modes each cover
// exactly one of the gaps claude_code_backend.go closes: "thinking" (a
// thinking content block plus the result event's own ttft_ms/duration_ms
// timing fields), "subagent" (a null-then-set parent_tool_use_id pair),
// "rate_limit_error"/"deterministic_error" (a result event this file's own
// claudeCodeRetryableClass must classify retryable/not-retryable,
// respectively), "crash"/"crash_before_init" (a child that exits nonzero
// with no "result" event, AFTER vs. BEFORE ever emitting a "system" event
// — runClaudeCodeTurn's waitErr branch must classify only the former
// retryable), "fast_no_drain" (closes its own stdin immediately,
// without ever draining it, to reliably win the race against harness's
// own turn-input write — see runClaudeCodeTurn's inputErr handling), and
// "rate_limit_event" (a subscription rate-limit/quota event ahead of the
// final assistant text, mapped by mapClaudeCodeRateLimit onto
// Session.SubscriptionUsage), "rate_limit_event_no_overage" (the same
// event with no overage in play at all — overageStatus "", isUsingOverage
// false, overageResetsAt 0 — proving mapClaudeCodeRateLimit leaves
// SubscriptionUsage.Overage nil rather than a hollow zero-value object),
// and "bg_leak" (reproduces a `claude --bg` turn whose detached child
// inherits this process's stdout AND stderr and outlives it — see its own
// comment below and claude_code_backend.go's consumeClaudeCodeStream/
// runClaudeCodeTurn doc comments for the wedge this proves fixed).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func main() {
	mode := os.Getenv("FAKE_CLAUDE_MODE")

	if mode == "bg_leak_child" {
		// The detached grandchild "bg_leak" mode spawns below, standing in
		// for `claude --bg`'s own daemon (or a further child of its own,
		// e.g. a dev server) that inherits the direct `claude` child's
		// stdout AND stderr and outlives it. This process emits nothing at
		// all — it exists solely to hold both inherited fds (this
		// process's own os.Stdout/os.Stderr, which ARE the harness-owned
		// pipes' write ends) open far past any sane test timeout, so a
		// test can prove the driver returns without ever waiting for
		// either fd's EOF. The test kills it by PID once it has proved
		// that.
		time.Sleep(time.Hour)
		return
	}

	if mode == "fast_no_drain" {
		// Close our OWN stdin's read end IMMEDIATELY — before doing
		// anything else, including the argv log write below — to
		// deterministically win the race this mode's own test regresses:
		// harness's own turn-input Write/Close must tolerate a broken-pipe/
		// closed-pipe error when the child has already (or is about to)
		// exit with a complete, valid result, exactly the shape a fast/
		// trivial real turn can hit. fakeclaude runs at native speed
		// (buildFakeClaude compiles it without -race); harness's own
		// -race-instrumented write path is comparatively slow, so closing
		// this early reliably beats it. Skips the shared stdin-drain
		// goroutine below entirely — there is nothing left to drain.
		_ = os.Stdin.Close()
	}

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

	if mode != "fast_no_drain" {
		// Read (and, when FAKE_CLAUDE_STDIN_LOG is set, record) the one
		// turn-input line the real driver writes — the real driver writes
		// its turn message and closes stdin BEFORE it ever reads this
		// process's stdout (see runClaudeCodeTurn's own stdin write/close
		// ordering, claude_code_backend.go), so a synchronous read-to-EOF
		// here always completes promptly regardless of mode, with none of
		// the goroutine-vs-process-exit race a background drain would
		// carry for a test that actually wants the captured bytes.
		// FAKE_CLAUDE_STDIN_LOG lets a test recover the EXACT bytes the
		// driver sent — e.g. proving a checked-out task notification's
		// rendered content actually reached the CLI's input, not just the
		// bare trigger text (engine/claude_code_backend_test.go).
		stdinBytes, _ := io.ReadAll(os.Stdin)
		if logPath := os.Getenv("FAKE_CLAUDE_STDIN_LOG"); logPath != "" {
			if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				_, _ = f.Write(stdinBytes)
				f.Close()
			}
		}
	}

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

	if mode == "crash_before_init" {
		// Exits nonzero WITHOUT ever emitting so much as a "system" event —
		// the deterministic-startup-failure shape (an unknown flag, a
		// malformed --mcp-config command, an invalid --model) that
		// runClaudeCodeTurn's waitErr branch must NOT classify retryable,
		// unlike "crash" below (which starts normally and dies later).
		os.Exit(1)
	}

	emit(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
	})

	switch mode {
	case "fast_no_drain":
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Done before you finished writing."},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "Done before you finished writing.",
			"usage": map[string]any{
				"input_tokens":  4,
				"output_tokens": 6,
			},
		})
		return
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
	case "crash":
		// Emitted "system"/"init" above, THEN exits nonzero without ever
		// emitting a "result" event — the non-deterministic mid-session
		// child-failure shape runClaudeCodeTurn's own waitErr branch must
		// classify retryable (a crash, not a deterministic domain failure
		// the CLI itself reported). Contrast crash_before_init above,
		// which never gets this far.
		os.Exit(1)
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
	case "rate_limit_error":
		emit(map[string]any{
			"type":     "result",
			"subtype":  "error_during_execution",
			"is_error": true,
			"result":   "rate_limit_error: please retry later",
			"usage": map[string]any{
				"input_tokens":  6,
				"output_tokens": 1,
			},
		})
		return
	case "deterministic_error":
		emit(map[string]any{
			"type":     "result",
			"subtype":  "error_max_turns",
			"is_error": true,
			"result":   "exceeded maximum turns",
			"usage": map[string]any{
				"input_tokens":  8,
				"output_tokens": 2,
			},
		})
		return
	case "thinking":
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "thinking", "thinking": "Let me reason about this.", "signature": "sig-abc"},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Here is my answer."},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "Here is my answer.",
			"usage": map[string]any{
				"input_tokens":  20,
				"output_tokens": 10,
			},
			"ttft_ms":     120,
			"duration_ms": 800,
		})
		return
	case "subagent":
		emit(map[string]any{
			"type":               "assistant",
			"parent_tool_use_id": nil,
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_parent", "name": "Task", "input": map[string]any{}},
				},
			},
		})
		emit(map[string]any{
			"type":               "assistant",
			"parent_tool_use_id": "toolu_parent",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Working inside the subagent."},
				},
			},
		})
		emit(map[string]any{
			"type":               "user",
			"parent_tool_use_id": "toolu_parent",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "toolu_parent", "content": "subagent done"},
				},
			},
		})
		emit(map[string]any{
			"type":               "assistant",
			"parent_tool_use_id": nil,
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "All done."},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "All done.",
			"usage": map[string]any{
				"input_tokens":  30,
				"output_tokens": 15,
			},
		})
		return
	case "bg_leak":
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Starting a background job."},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "Starting a background job.",
			"usage": map[string]any{
				"input_tokens":  12,
				"output_tokens": 5,
			},
		})
		// Simulate `claude --bg`'s detached child inheriting this
		// process's stdout AND stderr fds (the harness-owned pipes' write
		// ends) and outliving THIS direct child — see
		// claude_code_backend.go's consumeClaudeCodeStream and
		// runClaudeCodeTurn doc comments for the wedge this reproduces.
		// BOTH fds matter here, not just stdout: a real leaked descendant
		// (a dev server started by a background task, say) commonly
		// inherits its parent's whole stdio, not a hand-picked subset, so
		// a test that only leaks stdout would miss a driver that still
		// wedges through stderr alone. Spawn a grandchild that inherits
		// os.Stdout AND os.Stderr verbatim and sleeps well past any sane
		// test timeout, holding both pipes' write ends open long after
		// this process (the direct `claude` child harness itself manages)
		// exits right below. A minimal, explicit Env — not this process's
		// own os.Environ() — keeps the grandchild out of FAKE_CLAUDE_LOG's
		// invocation log, since it is not a `claude` invocation a test
		// should count. The test kills the grandchild by PID (recorded via
		// FAKE_CLAUDE_LEAK_PID_FILE) once it has proved the driver did not
		// wait for it.
		leaker := exec.Command(os.Args[0])
		leaker.Env = []string{"FAKE_CLAUDE_MODE=bg_leak_child"}
		leaker.Stdout = os.Stdout
		leaker.Stderr = os.Stderr
		if err := leaker.Start(); err == nil {
			if pidFile := os.Getenv("FAKE_CLAUDE_LEAK_PID_FILE"); pidFile != "" {
				_ = os.WriteFile(pidFile, []byte(strconv.Itoa(leaker.Process.Pid)), 0o644)
			}
			_ = leaker.Process.Release()
		}
		return
	}

	if mode == "rate_limit_event" || mode == "rate_limit_event_no_overage" {
		info := map[string]any{
			"status":          "allowed",
			"resetsAt":        1788785267,
			"rateLimitType":   "five_hour",
			"overageStatus":   "allowed",
			"overageResetsAt": 1789000000,
			"isUsingOverage":  false,
			"unifiedWindows": map[string]any{
				"five_hour": map[string]any{"utilization": 0.02, "resetsAt": 1788785267},
				"seven_day": map[string]any{"utilization": 0.13, "resetsAt": 1789200000},
			},
		}
		if mode == "rate_limit_event_no_overage" {
			// No overage in play at all — overageStatus empty,
			// isUsingOverage false, overageResetsAt 0 — the shape
			// mapClaudeCodeRateLimit must leave Overage nil for (see
			// TestClaudeCodeRateLimitEventWithNoOverageOmitsOverage), unlike
			// the "rate_limit_event" mode above, whose overageStatus:
			// "allowed" is itself a real (if benign) overage signal.
			info["overageStatus"] = ""
			info["overageResetsAt"] = 0
		}
		emit(map[string]any{
			"type":            "rate_limit_event",
			"rate_limit_info": info,
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Here is my answer."},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "Here is my answer.",
			"usage": map[string]any{
				"input_tokens":  9,
				"output_tokens": 4,
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
		"ttft_ms":        50,
		"duration_ms":    400,
	})
}
