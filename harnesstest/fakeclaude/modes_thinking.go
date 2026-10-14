package main

const reservedID = "cmpsum_fakeupstream"

// threadModes shape how harness groups thinking blocks into messages.
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
}
