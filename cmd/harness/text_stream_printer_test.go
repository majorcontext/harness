package main

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
)

// TestTextStreamPrinterTurnRestartBreaks is the red-first guard for the CLI
// plain-text renderer consuming EventTurnRestart. A base-loop retry
// re-streams a turn's partial text; without a restart case the two runs print
// concatenated inline as "Hello worHello world". textStreamPrinter must break
// to a fresh line so the retry text is never joined to the stale partial.
//
// Red-verify the NAMED mechanism: delete the EventTurnRestart case in
// textStreamPrinter.handle and stdout reads "Hello worHello world".
func TestTextStreamPrinterTurnRestartBreaks(t *testing.T) {
	var out, errW strings.Builder
	p := &textStreamPrinter{out: &out, errW: &errW}
	for _, ev := range []engine.Event{
		{Type: engine.EventTextDelta, Text: "Hello wor"},
		{Type: engine.EventTurnRestart},
		{Type: engine.EventTextDelta, Text: "Hello wor"},
		{Type: engine.EventTextDelta, Text: "ld"},
	} {
		p.handle(ev)
	}
	if got := out.String(); got != "Hello wor\nHello world" {
		t.Fatalf("stdout = %q, want %q (turn.restart must break, never concatenate)", got, "Hello wor\nHello world")
	}
	if !p.printedText {
		t.Error("printedText = false, want true after text was streamed")
	}
	if !strings.Contains(errW.String(), "re-streaming") {
		t.Errorf("errW = %q, want a re-stream notice", errW.String())
	}
}

// TestTextStreamPrinterRestartWithoutPartialAddsNoBreak proves the
// streamedThis gate: a restart on an attempt that printed no text (a stream
// that died before any delta) must not inject a spurious leading blank line.
func TestTextStreamPrinterRestartWithoutPartialAddsNoBreak(t *testing.T) {
	var out, errW strings.Builder
	p := &textStreamPrinter{out: &out, errW: &errW}
	p.handle(engine.Event{Type: engine.EventTurnRestart})
	p.handle(engine.Event{Type: engine.EventTextDelta, Text: "hi"})
	if got := out.String(); got != "hi" {
		t.Fatalf("stdout = %q, want %q (no leading blank line)", got, "hi")
	}
}
