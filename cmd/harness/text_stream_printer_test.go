package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
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

// TestRunOnEventHandlerSerializesConcurrentCallers is the regression test
// for a live review finding: a `task` child spawned from `harness run`
// runs its own Prompt goroutine concurrently with the parent's own
// top-level Prompt/PursueGoal call, and both invoke the SAME OnEvent
// callback (configSnapshot copies Config.OnEvent by value into every
// child's Config) — see newRunOnEventHandler's own doc comment for the
// full mechanism. Neither *json.Encoder.Encode nor
// textStreamPrinter.handle is safe for concurrent use on its own, so
// without newRunOnEventHandler's mutex this is a genuine data race:
// `go test -race` catches it directly (this test's whole point), and even
// without -race, two goroutines racing on textStreamPrinter's
// printedText/streamedThis fields and shared io.Writer calls corrupt
// output — interleaved text in the plain-text case, invalid concatenated
// JSON in the -json case (a bare json.Encoder.Encode is not atomic against
// a concurrent Encode: two goroutines can interleave their writes to the
// underlying io.Writer mid-value). Exercises the callback exactly the way
// a real parent+child pair would: many goroutines hammering it at once,
// for both the jsonOut and plain-text configurations.
func TestRunOnEventHandlerSerializesConcurrentCallers(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		handler := newRunOnEventHandler(&textStreamPrinter{}, enc, true)
		runConcurrentEvents(handler)
		// Every line the encoder wrote must be independently valid JSON —
		// a race that interleaved two Encode calls mid-value would produce
		// at least one line that fails to unmarshal.
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var ev engine.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("interleaved/corrupted JSON line: %q: %v", line, err)
			}
		}
	})
	t.Run("text", func(t *testing.T) {
		var out, errW strings.Builder
		printer := &textStreamPrinter{out: &out, errW: &errW}
		handler := newRunOnEventHandler(printer, json.NewEncoder(&bytes.Buffer{}), false)
		runConcurrentEvents(handler)
		// No specific output shape is asserted (concurrent delta ordering
		// is inherently unordered) — this run's real assertion is
		// `go test -race` itself finding nothing to report. Reaching here
		// at all, under -race, IS the pass condition.
	})
}

// TestTextStreamPrinterPrintedTextSafeDuringConcurrentHandle is the
// regression test for a second live review finding on the same fix:
// newRunOnEventHandler's mutex only serializes calls made THROUGH it, but
// runCmd's own tail (the trailing-newline check after its top-level Prompt
// call returns) used to read printer.printedText directly — and a `task`
// child's own background Prompt goroutine can still be calling handle
// after the parent's own top-level call has already returned (`task` is
// explicitly non-blocking; nothing waits for a child to finish first).
// printer.mu now guards both handle and this read (via PrintedText).
// Exercises exactly that shape: one goroutine hammering handle while
// another repeatedly calls PrintedText — go test -race is this test's
// real assertion.
func TestTextStreamPrinterPrintedTextSafeDuringConcurrentHandle(t *testing.T) {
	var out, errW strings.Builder
	p := &textStreamPrinter{out: &out, errW: &errW}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p.handle(engine.Event{Type: engine.EventTextDelta, Text: "x"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = p.PrintedText()
		}
	}()
	wg.Wait()
	if !p.PrintedText() {
		t.Error("PrintedText() = false after handle streamed text, want true")
	}
}

// runConcurrentEvents fires a burst of engine.Event values at handler from
// many goroutines at once, mirroring a real parent-turn-plus-several-
// children shape closely enough for -race to catch any missing
// synchronization in handler itself.
func runConcurrentEvents(handler func(engine.Event)) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				handler(engine.Event{Type: engine.EventTextDelta, Text: "x"})
				handler(engine.Event{Type: engine.EventMessage})
			}
		}(g)
	}
	wg.Wait()
}
