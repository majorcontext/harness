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
// timing fields), "thinking_interleaved" (text, then thinking, then the
// text that completes THAT thinking block's own turn segment — proves the
// reasoning-buffer merge in consumeClaudeCodeStream attaches only forward,
// never sweeping in an unrelated, already-flushed text message),
// "thinking_ratelimit_text" (thinking, then a rate_limit_event, then the
// text that completes the thinking block's own turn segment — proves the
// pre-switch flush guard does not treat content-free activity as ending
// the turn segment, which would re-split it), "thinking_then_crash" (a
// thinking block immediately followed by a nonzero exit with no "result"
// event — proves the buffered reasoning survives the post-loop flush
// instead of being dropped), "thinking_then_subagent" (a top-level
// thinking block immediately followed by an assistant envelope on a
// DIFFERENT parent_tool_use_id — proves the buffered reasoning flushes
// standalone instead of merging across threads), "subagent"
// (a null-then-set parent_tool_use_id pair),
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
// "bg_leak" (reproduces a `claude --bg` turn whose detached child
// inherits this process's stdout AND stderr and outlives it — see its own
// comment below and claude_code_backend.go's consumeClaudeCodeStream/
// runClaudeCodeTurn doc comments for the wedge this proves fixed), and
// "queue_injection" (blocks for a SECOND stdin line mid-turn, proving the
// driver's stdin-writer pump keeps stdin open and delivers a mid-turn
// queued prompt to THIS running child instead of a fresh one — see its own
// comment below), "queue_injection_broken_pipe" (closes its own stdin read
// end before the driver ever gets a chance to write a mid-turn injection,
// so that write fails — proves a failed injection's watermark accounting
// does not silently strand it, see its own comment below), and
// "queue_injection_blocked_write" (never reads stdin again after its own
// first marker, so a large mid-turn injection fills the pipe and blocks
// the driver's own Write — proves a stop landing mid-write still retires
// the pump promptly instead of wedging, see its own comment below).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

	// stdinR is kept OPEN and read line-by-line for the rest of main(),
	// instead of the one-shot read-to-EOF this file used before the
	// driver started keeping its own stdin open across a whole turn (see
	// claude_code_backend.go's runClaudeCodeTurn doc comment on the
	// stdin-writer pump it mirrors from the Claude Agent SDK's
	// Query.streamInput). The real `claude` binary reads its input as a
	// stream of newline-delimited JSON lines, not a single blob ending in
	// EOF — an io.ReadAll here would now block forever, since the driver
	// no longer closes stdin right after its first write. readStdinLine
	// mirrors that real streaming-read shape: one line per call, blocking
	// until it arrives (or stdin closes), which is also what lets the
	// "queue_injection" mode below block for a SECOND line while the
	// first turn is still open, exactly the mid-turn steering window this
	// stand-in exists to prove.
	stdinR := bufio.NewReader(os.Stdin)
	readStdinLine := func() (line string, ok bool) {
		b, err := stdinR.ReadString('\n')
		if logPath := os.Getenv("FAKE_CLAUDE_STDIN_LOG"); logPath != "" && b != "" {
			// Record (append) exactly the bytes read, same shape the old
			// one-shot read-to-EOF left behind — a test recovers the
			// EXACT bytes the driver sent on each line, e.g. proving a
			// checked-out task notification's rendered content actually
			// reached the CLI's input (engine/claude_code_backend_test.go).
			if f, ferr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
				_, _ = f.WriteString(b)
				f.Close()
			}
		}
		return strings.TrimRight(b, "\n"), err == nil
	}

	if mode != "fast_no_drain" {
		// Read the ONE turn-input line every mode's own first message
		// carries. Every mode below that does not itself read a further
		// line (i.e. every mode except "queue_injection") never calls
		// readStdinLine again, so its own stdin simply sits unread (but
		// still open) until the driver eventually closes it — harmless,
		// mirroring how the real CLI ignores stdin it has no more use for
		// mid-turn.
		readStdinLine()
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
	case "queue_injection":
		// Proves the driver keeps stdin OPEN across a turn and delivers a
		// prompt queued mid-turn as a SECOND stream-json input line to
		// THIS SAME running child, rather than closing stdin right after
		// the first write (the pre-fix shape) or waiting until the whole
		// turn ends. Emits a "WAITING_FOR_QUEUE" marker the test's OnEvent
		// hook waits for (a deterministic, non-sleep synchronization
		// point: the driver has finished mapping this event, so the child
		// is now blocked in readStdinLine for line two) before the test
		// enqueues a prompt. If a second line never arrives — a driver
		// that still closes stdin after line one sees immediate EOF here,
		// not a hang, since a closed pipe's read returns right away —
		// this reports a distinguishable result instead of echoing
		// anything, so the red run fails on content, not a timeout.
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": []map[string]any{{"type": "text", "text": "WAITING_FOR_QUEUE"}},
			},
		})
		line, ok := readStdinLine()
		resultText := "no second message received"
		if ok {
			var second struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(line), &second); err == nil {
				resultText = "received queued: " + second.Message.Content
				emit(map[string]any{
					"type": "assistant",
					"message": map[string]any{
						"role":    "assistant",
						"content": []map[string]any{{"type": "text", "text": resultText}},
					},
				})
			}
		}
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   resultText,
			"usage": map[string]any{
				"input_tokens":  5,
				"output_tokens": 5,
			},
		})
		return
	case "queue_injection_broken_pipe":
		// Proves the driver's own watermark bookkeeping never advances
		// PAST a mid-turn injection whose stdin write actually failed --
		// the shape a real child's read end closing right as the
		// injection lands produces (EPIPE on a `claude --bg` turn, or any
		// child that exits between the wake firing and the write
		// happening). Emits WAITING_FOR_QUEUE (the same synchronization
		// marker "queue_injection" uses), then closes ITS OWN stdin read
		// end and emits a SECOND marker, STDIN_CLOSED_READY, only after
		// that close has actually completed -- the test waits for THIS
		// marker (not WAITING_FOR_QUEUE) before enqueueing, so the
		// driver's injection write is guaranteed to land on an
		// already-closed pipe and fail, deterministically, never a race
		// against whether the close beat the write. Never reads a second
		// line at all -- there is nothing left open to read from -- and
		// completes normally, exactly like a turn that finished its own
		// work with no idea a queued prompt ever existed.
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": []map[string]any{{"type": "text", "text": "WAITING_FOR_QUEUE"}},
			},
		})
		_ = os.Stdin.Close()
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": []map[string]any{{"type": "text", "text": "STDIN_CLOSED_READY"}},
			},
		})
		// Give the driver's stdin-writer pump time to actually wake,
		// dequeue, and attempt (and fail) its write against the
		// now-closed pipe before this process finishes the turn --
		// otherwise a fast finish could race ahead of the test's own
		// EnqueuePrompt call and leave nothing for the pump to even
		// attempt. This is cross-process timing against a real
		// subprocess (see e2e/AGENTS.md's carve-out for exactly this
		// class of wait), not a substitute for the Go test's own
		// event-driven synchronization, which never sleeps.
		time.Sleep(300 * time.Millisecond)
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "STDIN_CLOSED_READY",
			"usage": map[string]any{
				"input_tokens":  5,
				"output_tokens": 5,
			},
		})
		return
	case "queue_injection_blocked_write":
		// Proves a stop landing while the driver's stdin-writer pump is
		// BLOCKED inside its own stdin.Write call still retires it
		// promptly, instead of wedging <-pumpDone (and so the whole
		// turn) until ctx cancellation -- the shape a `claude --bg`
		// leaked grandchild holding stdin's read end open produces. This
		// mirrors "bg_leak" below exactly, just on stdin instead of
		// stdout/stderr: spawn a grandchild that inherits THIS process's
		// stdin (a DUPLICATED copy of the same pipe read end) and sleeps
		// well past any sane test timeout, then let THIS direct child
		// exit normally and quickly right after its own "result" --
		// cmd.Wait() on the direct child returns promptly either way (the
		// existing EOF-avoidance design this file's own doc comment
		// covers), but the pipe's read end stays held open by the
		// grandchild regardless of the direct child's own lifetime, so
		// ONLY an explicit stdin.Close() on the driver's side can ever
		// unblock a write still in flight against it. The test kills the
		// grandchild by PID (FAKE_CLAUDE_LEAK_PID_FILE, same convention
		// "bg_leak" uses) once it has proved the driver did not wedge on
		// it.
		//
		// Emits WAITING_FOR_QUEUE, spawns the leaker, then gives the
		// driver's own mid-turn injection write (the test enqueues
		// something many times larger than any real pipe buffer) time to
		// actually start and block inside the OS write(2) call before
		// this process emits "result" and exits -- cross-process timing
		// against a real subprocess, not a substitute for the Go test's
		// own event-driven synchronization (see e2e/AGENTS.md's carve-out
		// for this class of wait).
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": []map[string]any{{"type": "text", "text": "WAITING_FOR_QUEUE"}},
			},
		})
		leaker := exec.Command(os.Args[0])
		leaker.Env = []string{"FAKE_CLAUDE_MODE=bg_leak_child"}
		leaker.Stdin = os.Stdin
		if err := leaker.Start(); err == nil {
			if pidFile := os.Getenv("FAKE_CLAUDE_LEAK_PID_FILE"); pidFile != "" {
				_ = os.WriteFile(pidFile, []byte(strconv.Itoa(leaker.Process.Pid)), 0o644)
			}
			_ = leaker.Process.Release()
		}
		time.Sleep(500 * time.Millisecond)
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "done despite a blocked mid-turn write",
			"usage": map[string]any{
				"input_tokens":  5,
				"output_tokens": 5,
			},
		})
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
	case "parallel_tools":
		// One upstream API response carrying a thinking block and TWO
		// parallel tool_use blocks, streamed the way a real `claude`
		// 2.1.251 binary streams it (verified live): one envelope per
		// content block, every envelope repeating the SAME upstream
		// message.id, and the FIRST tool's result interleaved before the
		// second tool_use envelope is sent. A separate response with its
		// own id closes the turn, so the test can prove the grouping ends
		// at the id boundary rather than swallowing everything.
		const parallelID = "msg_011FAKEPARALLEL"
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":   parallelID,
				"role": "assistant",
				"content": []map[string]any{
					{"type": "thinking", "thinking": "Two commands, one response.", "signature": "sig-par"},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":   parallelID,
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_alpha", "name": "Bash", "input": map[string]any{"command": "echo alpha"}},
				},
			},
		})
		emit(map[string]any{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "toolu_alpha", "content": "alpha"},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":   parallelID,
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_beta", "name": "Bash", "input": map[string]any{"command": "echo beta"}},
				},
			},
		})
		emit(map[string]any{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "toolu_beta", "content": "beta"},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":   "msg_011FAKEFINAL",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "done"},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "done",
			"usage": map[string]any{
				"input_tokens":  20,
				"output_tokens": 10,
			},
		})
		return
	case "parallel_tools_crossing":
		// The two boundary cases the grouping must not cross, in one
		// stream. A SUBAGENT response calls a tool on its own
		// parent_tool_use_id thread and is journaled when the main
		// thread's response opens; its tool_result then arrives while
		// that main-thread response is still being assembled, so it
		// answers a call that IS already journaled and must not be held
		// behind an unrelated response. Then a THINKING-ONLY envelope
		// opens the next response: its id differs, so it must close the
		// open one rather than slip past on the reasoning-buffer path.
		const subagentID = "msg_011FAKECROSSSUB"
		const firstID = "msg_011FAKECROSSA"
		const secondID = "msg_011FAKECROSSB"
		emit(map[string]any{
			"type":               "assistant",
			"parent_tool_use_id": "toolu_parent",
			"message": map[string]any{
				"id":   subagentID,
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_child", "name": "Bash", "input": map[string]any{"command": "echo child"}},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":   firstID,
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_main", "name": "Bash", "input": map[string]any{"command": "echo main"}},
				},
			},
		})
		emit(map[string]any{
			"type":               "user",
			"parent_tool_use_id": "toolu_parent",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "toolu_child", "content": "child"},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":   secondID,
				"role": "assistant",
				"content": []map[string]any{
					{"type": "thinking", "thinking": "Next response opens with thinking.", "signature": "sig-cross"},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":   secondID,
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "done"},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "done",
			"usage": map[string]any{
				"input_tokens":  20,
				"output_tokens": 10,
			},
		})
		return
	case "thinking_interleaved":
		// Proves the reasoning-buffer merge (claude_code_backend.go's
		// pendingReasoning) attaches ONLY forward, to the envelope that
		// immediately follows a thinking block, never backward onto an
		// unrelated text envelope that preceded it — i.e. a
		// text-reasoning-text turn must not collapse into one giant
		// message. Sequence: independent text, then a thinking block,
		// then the text that completes ITS OWN turn segment.
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "First, a quick note."},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "thinking", "thinking": "Now let me reason about the rest.", "signature": "sig-def"},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "And here is the rest."},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "And here is the rest.",
			"usage": map[string]any{
				"input_tokens":  30,
				"output_tokens": 15,
			},
		})
		return
	case "thinking_ratelimit_text":
		// Proves the pre-switch flush guard in consumeClaudeCodeStream
		// does NOT flush a buffered thinking block on content-free
		// activity: a "rate_limit_event" (the CLI's own subscription
		// signal, which its own doc comment says can arrive mid-turn,
		// shifting limits) lands BETWEEN the thinking envelope and the
		// text envelope that completes its own turn segment. The result
		// must still be ONE merged [Reasoning, Text] message, exactly
		// like the "thinking" mode above — a guard that flushes on every
		// non-"assistant" envelope re-splits the turn right here.
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "thinking", "thinking": "Reasoning across a rate-limit event.", "signature": "sig-rl"},
				},
			},
		})
		emit(map[string]any{
			"type": "rate_limit_event",
			"rate_limit_info": map[string]any{
				"status":        "allowed",
				"resetsAt":      1788785267,
				"rateLimitType": "five_hour",
				"unifiedWindows": map[string]any{
					"five_hour": map[string]any{"utilization": 0.02, "resetsAt": 1788785267},
				},
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "Here is my answer after the rate-limit event."},
				},
			},
		})
		emit(map[string]any{
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "Here is my answer after the rate-limit event.",
			"usage": map[string]any{
				"input_tokens":  22,
				"output_tokens": 11,
			},
		})
		return
	case "thinking_then_crash":
		// Proves flushPendingReasoning's post-loop flush: a thinking
		// block arrives, then the child crashes (nonzero exit, no
		// "result" event at all) — the buffered reasoning must still
		// survive as a standalone assistant message rather than being
		// silently dropped when consumeClaudeCodeStream's scanner loop
		// ends via EOF. Mirrors the plain "crash" mode above, but with a
		// thinking block emitted first.
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "thinking", "thinking": "Reasoning right before a crash.", "signature": "sig-crash"},
				},
			},
		})
		os.Exit(1)
	case "thinking_then_subagent":
		// Proves flushPendingReasoning's different-parent flush: a
		// top-level (parent_tool_use_id "") thinking block is immediately
		// followed by an assistant envelope belonging to a DIFFERENT
		// thread (a subagent's own parent_tool_use_id) — the buffered
		// reasoning must flush standalone rather than merge onto content
		// from a different thread.
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "thinking", "thinking": "Reasoning about which subagent to spawn.", "signature": "sig-subagent"},
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
			"type":     "result",
			"subtype":  "success",
			"is_error": false,
			"result":   "Working inside the subagent.",
			"usage": map[string]any{
				"input_tokens":  18,
				"output_tokens": 9,
			},
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
