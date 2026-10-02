package main

const (
	reservedID = "cmpsum_fakeupstream"
	parallelID = "msg_011FAKEPARALLEL"
	finalID    = "msg_011FAKEFINAL"
	crossSubID = "msg_011FAKECROSSSUB"
	crossAID   = "msg_011FAKECROSSA"
	crossBID   = "msg_011FAKECROSSB"
)

func (o obj) parent(id any) obj { return o.set("parent_tool_use_id", id) }

var rateLimitMidThinking = rateLimitEvent(obj{
	"status": "allowed", "resetsAt": 1788785267, "rateLimitType": "five_hour",
	"unifiedWindows": obj{"five_hour": obj{"utilization": 0.02, "resetsAt": 1788785267}},
})

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
	"thinking_interleaved": frames(
		say("First, a quick note."),
		assistant(thinkingBlock("Now let me reason about the rest.", "sig-def")),
		say("And here is the rest."),
		success("And here is the rest.", 30, 15),
	),
	"thinking_ratelimit_text": frames(
		assistant(thinkingBlock("Reasoning across a rate-limit event.", "sig-rl")),
		rateLimitMidThinking,
		say("Here is my answer after the rate-limit event."),
		success("Here is my answer after the rate-limit event.", 22, 11),
	),
	"thinking_then_crash": crashAfter(assistant(thinkingBlock("Reasoning right before a crash.", "sig-crash"))),
	"thinking_then_subagent": frames(
		assistant(thinkingBlock("Reasoning about which subagent to spawn.", "sig-subagent")),
		say("Working inside the subagent.").parent("toolu_parent"),
		success("Working inside the subagent.", 18, 9),
	),
	"parallel_tools": frames(
		assistant(thinkingBlock("Two commands, one response.", "sig-par")).inMessage("id", parallelID),
		assistant(toolUse("toolu_alpha", "Bash", obj{"command": "echo alpha"})).inMessage("id", parallelID),
		user(toolResult("toolu_alpha", "alpha", false)),
		assistant(toolUse("toolu_beta", "Bash", obj{"command": "echo beta"})).inMessage("id", parallelID),
		user(toolResult("toolu_beta", "beta", false)),
		say("done").inMessage("id", finalID),
		success("done", 20, 10),
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
