package main

import (
	"encoding/json"
	"os"
	"time"
)

const waitingMarker = "WAITING_FOR_QUEUE"

// stdinModes print a marker and then act on the input stream or on the
// inherited stdio pipes, so a test can check how the driver treats them.
var stdinModes = map[string]mode{
	"queue_injection": queueInjection,
	"steer":           steer,
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

// awaitQueued reads one queued message from stdin. It stops waiting when the
// test opens the window gate, so a test that asserts no message arrives
// closes the window itself instead of waiting out a clock. A test that never
// opens the gate gets an error result and a non-zero exit after a bound.
func awaitQueued(f *fake) (string, bool) {
	line := make(chan string, 1)
	go func() {
		if l, ok := f.readLine(); ok {
			line <- l
		}
	}()
	for deadline := time.Now().Add(gateWait); time.Now().Before(deadline); time.Sleep(gatePoll) {
		select {
		case l := <-line:
			return queuedContent(l)
		default:
		}
		if gateOpen(windowGateFile) {
			select {
			case l := <-line:
				return queuedContent(l)
			default:
				return "", false
			}
		}
	}
	f.emit(failed(0, 0, "window gate never opened"))
	os.Exit(1)
	return "", false
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
