package main

import (
	"encoding/json"
	"os"
	"time"
)

const finalTextMarker = "FINAL_TEXT"

const waitingMarker = "WAITING_FOR_QUEUE"

// stdinModes print a marker and then act on the input stream or on the
// inherited stdio pipes, so a test can check how the driver treats them.
var stdinModes = map[string]mode{
	"queue_injection":      queueInjection,
	"steer":                steer,
	"steer_final_text":     steerFinalText,
	"steer_unanswered":     steerUnanswered,
	"steer_hang":           steerHang,
	"steer_untaken":        steerUntaken,
	"steer_text_then_tool": steerTextThenTool,
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

// slowTool streams a response that holds one bash call and ends the response,
// as the CLI does before it runs the tool.
func slowTool(f *fake, id string) {
	call := toolUse(id, "mcp__harness__bash", obj{"command": "sleep 1"})
	msg := "msg_" + id
	partialEmit(f, messageStart(msg), blockStart(0, call))
	f.emit(assistant(call).inMessage("id", msg))
	partialEmit(f, blockStop(0), messageStop("tool_use"), streamEvent(obj{"type": "message_stop"}))
}

// steer runs a tool and reads one input line while the tool runs, as the
// CLI takes a queued message at the next tool result. It answers with the
// queued text.
func steer(f *fake) {
	slowTool(f, "toolu_s")
	text := "no steer received"
	if content, ok := awaitQueued(f); ok {
		text = "steered: " + content
	}
	f.emit(user(toolResult("toolu_s", "slept", false)), say(text), success(text, 5, 5))
}

// answerQueued answers a message that the CLI queued past its result.
func answerQueued(f *fake, text string, queued bool) {
	if queued {
		answer := "answered: " + text
		f.emit(say(answer), success(answer, 5, 5))
	}
}

// steerFinalText streams a text-only reply and ends the turn at once, as the
// CLI does, then answers a message that arrived meanwhile in a second turn.
// A run that starts with the message answers it.
func steerFinalText(f *fake) {
	if content, ok := queuedContent(f.first); ok && content != "run it" {
		answerQueued(f, content, true)
		return
	}
	f.emit(say(finalTextMarker))
	text, queued := awaitQueued(f)
	f.emit(success(finalTextMarker, 5, 5))
	answerQueued(f, text, queued)
}

// steerUntaken ends the turn with its tool call unanswered after it took a
// message.
func steerUntaken(f *fake) {
	slowTool(f, "toolu_r")
	text, queued := awaitQueued(f)
	f.emit(say("tool done"), success("tool done", 5, 5).set("total_cost_usd", 0.01))
	if queued {
		answer := "answered: " + text
		f.emit(say(answer), success(answer, 5, 5).set("total_cost_usd", 0.03))
	}
}

// steerTextThenTool waits for the child gate between its text and its tool.
func steerTextThenTool(f *fake) {
	f.emit(say(finalTextMarker))
	if content, _ := queuedContent(f.first); content == "run it" {
		awaitGate()
	}
	steer(f)
}

// steerUnanswered takes a message, ends the turn with its result, and exits
// without a second result.
func steerUnanswered(f *fake) {
	slowTool(f, "toolu_r")
	awaitQueued(f)
	f.emit(say("tool done"), success("tool done", 5, 5))
}

// steerHang takes a message, ends the turn with its result, and then hangs
// as a CLI does while it starts the second turn.
func steerHang(f *fake) {
	steerUnanswered(f)
	hang(f)
}
