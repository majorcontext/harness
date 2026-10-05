package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const waitingMarker = "WAITING_FOR_QUEUE"

// stdinModes print a marker and then act on the input stream or on the
// inherited stdio pipes, so a test can check how the driver treats them.
var stdinModes = map[string]mode{
	"queue_injection":               queueInjection,
	"compact_queue_injection":       compactQueueInjection,
	"queue_injection_broken_pipe":   queueInjectionBrokenPipe,
	"queue_injection_blocked_write": queueInjectionBlockedWrite,
	"bg_leak":                       bgLeak,
	"steer":                         steer,
}

func queuedContent(line string) (string, bool) {
	var m struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &m) != nil {
		return "", false
	}
	return m.Message.Content, true
}

func queueResult(text string) obj { return success(text, 5, 5) }

// queueInjection blocks for a second input line. A driver that closes stdin
// after the first line gets an immediate end of input and the "no second
// message" result.
func queueInjection(f *fake) {
	f.emit(say(waitingMarker))
	text := "no second message received"
	if line, ok := f.readLine(); ok {
		if content, ok := queuedContent(line); ok {
			text = "received queued: " + content
			f.emit(say(text))
		}
	}
	f.emit(queueResult(text))
}

// compactQueueInjection waits a bounded time, because a delegated compact
// turn registers no wake channel and so never delivers a second line.
func compactQueueInjection(f *fake) {
	f.emit(say(waitingMarker))
	text := "no second message received"
	if content, ok := awaitQueued(f); ok {
		text = "received queued: " + content
	}
	f.emit(queueResult(text))
}

// awaitQueued reads one queued message from stdin. It gives up after a
// bound, so a driver that never writes fails the test instead of hanging it.
func awaitQueued(f *fake) (string, bool) {
	line := make(chan string, 1)
	go func() {
		if l, ok := f.readLine(); ok {
			line <- l
		}
	}()
	select {
	case l := <-line:
		return queuedContent(l)
	case <-time.After(3 * time.Second):
		return "", false
	}
}

// queueInjectionBrokenPipe closes its own stdin before announcing it, so the
// driver's injection write must fail.
func queueInjectionBrokenPipe(f *fake) {
	f.emit(say(waitingMarker))
	_ = os.Stdin.Close()
	f.emit(say("STDIN_CLOSED_READY"))
	time.Sleep(300 * time.Millisecond)
	f.emit(queueResult("STDIN_CLOSED_READY"))
}

func spawnLeaker(setup func(*exec.Cmd)) {
	leaker := exec.Command(os.Args[0])
	leaker.Env = []string{"FAKE_CLAUDE_MODE=bg_leak_child"}
	setup(leaker)
	if err := leaker.Start(); err != nil {
		return
	}
	if pidFile := os.Getenv("FAKE_CLAUDE_LEAK_PID_FILE"); pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(leaker.Process.Pid)), 0o644)
	}
	_ = leaker.Process.Release()
}

// queueInjectionBlockedWrite leaks a grandchild that holds the stdin read
// end open, so only the driver closing stdin can unblock its pending write.
func queueInjectionBlockedWrite(f *fake) {
	f.emit(say(waitingMarker))
	spawnLeaker(func(c *exec.Cmd) { c.Stdin = os.Stdin })
	time.Sleep(500 * time.Millisecond)
	f.emit(queueResult("done despite a blocked mid-turn write"))
}

// bgLeak leaks a grandchild that holds the stdout and stderr pipes open
// after this process exits.
func bgLeak(f *fake) {
	f.emit(say("Starting a background job."), success("Starting a background job.", 12, 5))
	spawnLeaker(func(c *exec.Cmd) {
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
	})
}

// steer runs a tool and reads one input line while the tool runs, as the
// CLI takes a queued message at the next tool result. It answers with the
// queued text.
func steer(f *fake) {
	f.emit(assistant(toolUse("toolu_s", "Bash", obj{"command": "sleep 1"})))
	text := "no steer received"
	if content, ok := awaitQueued(f); ok {
		text = "steered: " + content
	}
	f.emit(user(toolResult("toolu_s", "slept", false)), say(text), success(text, 5, 5))
}
