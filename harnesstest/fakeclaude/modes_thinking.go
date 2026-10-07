package main

const (
	reservedID = "cmpsum_fakeupstream"
	crossSubID = "msg_011FAKECROSSSUB"
	crossAID   = "msg_011FAKECROSSA"
	crossBID   = "msg_011FAKECROSSB"
)

func (o obj) parent(id any) obj { return o.set("parent_tool_use_id", id) }

// threadModes shape how harness groups thinking blocks, parallel tool calls,
// and subagent threads into messages.
var threadModes = map[string]mode{
	"thinking": frames(
		assistant(thinkingBlock("Let me reason about this.", "sig-abc")),
		say("Here is my answer."),
		success("Here is my answer.", 20, 10).set("ttft_ms", 120).set("duration_ms", 800),
	),
	"thinking_reserved_id": frames(
		assistant(thinkingBlock("Reasoning under a reserved id.", "sig-reserved")).inMessage("id", reservedID),
		say("Answer under the same reserved id.").inMessage("id", reservedID),
		success("Answer under the same reserved id.", 18, 9),
	),
	"parallel_tools_crossing": frames(
		assistant(toolUse("toolu_child", "Bash", obj{"command": "echo child"})).inMessage("id", crossSubID).parent("toolu_parent"),
		assistant(toolUse("toolu_main", "Bash", obj{"command": "echo main"})).inMessage("id", crossAID),
		user(toolResult("toolu_child", "child", false)).parent("toolu_parent"),
		assistant(thinkingBlock("Next response opens with thinking.", "sig-cross")).inMessage("id", crossBID),
		say("done").inMessage("id", crossBID),
		success("done", 20, 10),
	),
	"subagent": frames(
		assistant(toolUse("toolu_parent", "Task", obj{})).parent(nil),
		say("Working inside the subagent.").parent("toolu_parent"),
		user(toolResult("toolu_parent", "subagent done", false)).parent("toolu_parent"),
		say("All done.").parent(nil),
		success("All done.", 30, 15),
	),
}
