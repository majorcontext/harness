package main

import (
	"os"
	"slices"
)

const (
	partialFirstID  = "msg_011FAKEPARTIAL1"
	partialSecondID = "msg_011FAKEPARTIAL2"
	partialTaskID   = "msg_011FAKEPARTIALTASK"
)

// partialEmit prints frames that exist only with --include-partial-messages.
func partialEmit(f *fake, fs ...obj) {
	if slices.Contains(os.Args, "--include-partial-messages") {
		f.emit(fs...)
	}
}

// partialModes print the stream_event frames that the CLI prints with
// --include-partial-messages. Without the flag they print the assistant
// frames alone, as the CLI does.
var partialModes = map[string]mode{
	"partial_messages": partialMessages,
	"partial_crossing": partialCrossing,
	"partial_retry":    partialRetry,
}

func streamEvent(ev obj) obj {
	return obj{"type": "stream_event", "event": ev, "session_id": "fake-session-1", "parent_tool_use_id": nil}
}

func messageStart(id string) obj {
	return streamEvent(obj{"type": "message_start", "message": obj{
		"id": id, "type": "message", "role": "assistant", "content": []obj{}, "model": "claude-haiku-4-5-20251001"}})
}

func blockStart(index int, block obj) obj {
	return streamEvent(obj{"type": "content_block_start", "index": index, "content_block": block})
}

func blockDelta(index int, delta obj) obj {
	return streamEvent(obj{"type": "content_block_delta", "index": index, "delta": delta})
}

func blockStop(index int) obj { return streamEvent(obj{"type": "content_block_stop", "index": index}) }

func messageStop(reason string) obj {
	return streamEvent(obj{"type": "message_delta", "delta": obj{"stop_reason": reason, "stop_sequence": nil}})
}

func thinkingDelta(s string) obj {
	return obj{"type": "thinking_delta", "thinking": s}
}

func textDelta(s string) obj { return obj{"type": "text_delta", "text": s} }

// partialMessages prints one API response with thinking, text, and a tool
// call, then a second with text. Each block streams in pieces and then comes
// as an assistant frame, the way the CLI prints it.
func partialMessages(f *fake) {
	emit := func(fs ...obj) { partialEmit(f, fs...) }
	bash := toolUse("toolu_partial", "Bash", obj{"command": "echo hi"})
	emit(messageStart(partialFirstID),
		blockStart(0, thinkingBlock("", "")),
		blockDelta(0, thinkingDelta("Let me ")), blockDelta(0, thinkingDelta("reason.")),
		blockDelta(0, obj{"type": "signature_delta", "signature": "sig-partial"}))
	f.emit(assistant(thinkingBlock("Let me reason.", "sig-partial")).inMessage("id", partialFirstID))
	emit(blockStop(0), blockStart(1, textBlock("")),
		blockDelta(1, textDelta("Let me ")), blockDelta(1, textDelta("check ")), blockDelta(1, textDelta("that.")))
	f.emit(say("Let me check that.").inMessage("id", partialFirstID))
	emit(blockStop(1), blockStart(2, toolUse("toolu_partial", "Bash", obj{})),
		blockDelta(2, obj{"type": "input_json_delta", "partial_json": ""}))
	f.emit(assistant(bash).inMessage("id", partialFirstID))
	emit(blockStop(2), messageStop("tool_use"), streamEvent(obj{"type": "message_stop"}))
	f.emit(user(toolResult("toolu_partial", "hi\n", false)))
	emit(messageStart(partialSecondID), blockStart(0, textBlock("")),
		blockDelta(0, textDelta("It printed ")), blockDelta(0, textDelta("hi.")))
	f.emit(say("It printed hi.").inMessage("id", partialSecondID))
	emit(blockStop(0), messageStop("end_turn"), streamEvent(obj{"type": "message_stop"}))
	f.emit(success("It printed hi.", 12, 8))
}

// partialCrossing runs a subagent while the main thread streams. The main
// message holds a thinking block and a text block; the assistant frames of
// the subagent arrive in the middle of the text, as the CLI prints the frames
// of a background subagent. The stream events carry no subagent text.
func partialCrossing(f *fake) {
	emit := func(fs ...obj) { partialEmit(f, fs...) }
	f.emit(assistant(toolUse("toolu_parent", "Task", obj{})).inMessage("id", partialTaskID).parent(nil))
	emit(messageStart(partialFirstID), blockStart(0, thinkingBlock("", "")),
		blockDelta(0, thinkingDelta("Main ")), blockDelta(0, thinkingDelta("thinking.")))
	f.emit(assistant(thinkingBlock("Main thinking.", "sig-cross")).inMessage("id", partialFirstID))
	emit(blockStop(0), blockStart(1, textBlock("")), blockDelta(1, textDelta("Main ")))
	f.emit(say("Subagent text.").parent("toolu_parent"))
	emit(blockDelta(1, textDelta("text.")))
	f.emit(say("Main text.").inMessage("id", partialFirstID))
	emit(blockStop(1), messageStop("end_turn"), streamEvent(obj{"type": "message_stop"}))
	f.emit(user(toolResult("toolu_parent", "subagent done", false)).parent("toolu_parent"),
		success("Main text.", 14, 6))
}

// partialRetry drops a reply that it began and answers again, as the CLI does
// when it retries a call after a failed stream: the first message has no
// assistant frame.
func partialRetry(f *fake) {
	emit := func(fs ...obj) { partialEmit(f, fs...) }
	emit(messageStart(partialFirstID), blockStart(0, textBlock("")),
		blockDelta(0, textDelta("Dropped ")), blockDelta(0, textDelta("reply")),
		blockStop(0), messageStop("end_turn"), streamEvent(obj{"type": "message_stop"}),
		messageStart(partialSecondID), blockStart(0, textBlock("")),
		blockDelta(0, textDelta("Second ")), blockDelta(0, textDelta("try.")))
	f.emit(say("Second try.").inMessage("id", partialSecondID))
	emit(blockStop(0), messageStop("end_turn"), streamEvent(obj{"type": "message_stop"}))
	f.emit(success("Second try.", 9, 4))
}
