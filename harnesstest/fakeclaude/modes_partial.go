package main

import (
	"os"
	"slices"
)

const (
	partialFirstID  = "msg_011FAKEPARTIAL1"
	partialSecondID = "msg_011FAKEPARTIAL2"
)

// partialModes print the stream_event frames that the CLI prints with
// --include-partial-messages. Without the flag they print the assistant
// frames alone, as the CLI does.
var partialModes = map[string]mode{
	"partial_messages": partialMessages,
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
	partial := slices.Contains(os.Args, "--include-partial-messages")
	emit := func(fs ...obj) {
		if partial {
			f.emit(fs...)
		}
	}
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
