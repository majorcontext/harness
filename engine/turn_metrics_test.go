package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// stepClock returns a fixed, pre-scripted sequence of instants, one per
// call, panicking if exhausted. This is the fake clock Config.Now expects a
// test to inject (see Config.Now's doc comment): the events a scriptedStream
// yields have no real wall-clock gap between them, so real time.Now calls a
// nanosecond apart would compute a TTFT/stream duration of ~0 and prove
// nothing. Scripting the exact instants streamTurn's three Now() call sites
// (sentAt, firstDeltaAt, doneAt — see streamTurn in engine.go) observe lets a
// test assert an exact, non-zero TTFTMillis/StreamMillis.
type stepClock struct {
	times []time.Time
	i     int
}

func (c *stepClock) now() time.Time {
	if c.i >= len(c.times) {
		panic("stepClock: exhausted")
	}
	t := c.times[c.i]
	c.i++
	return t
}

// TestTurnMetricsComputesLatencyAndUsage is the red-first guard for the
// turn_metrics emit: one completed model call must report TTFTMillis (send
// to first content delta) and StreamMillis (first delta to EventDone)
// computed from the injected clock, plus every usage field (including
// prompt-cache read/write tokens) passed through verbatim from
// provider.Usage, and SystemLen/ToolsCount matching the exact request the
// provider received — the join key against the server's request.meta
// record (see TurnMetrics's doc comment).
//
// The Attempt/latch mechanisms this metric also depends on are red-verified
// by their own dedicated tests: TestTurnMetricsRecordsRetryAttempt (attempt
// propagation) and TestTurnMetricsFirstDeltaLatches (the firstDeltaAt gate) —
// see their doc comments for the exact reverts proven to fail.
func TestTurnMetricsComputesLatencyAndUsage(t *testing.T) {
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := &stepClock{times: []time.Time{
		base,                             // sentAt, just before prov.Stream
		base.Add(120 * time.Millisecond), // firstDeltaAt, the text_delta event
		base.Add(500 * time.Millisecond), // doneAt, EventDone
	}}

	usage := provider.Usage{
		InputTokens:      321,
		OutputTokens:     45,
		CacheReadTokens:  200,
		CacheWriteTokens: 12,
	}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		{
			{Type: provider.EventTextDelta, Text: "hi"},
			{
				Type:       provider.EventDone,
				Message:    &message.Message{ID: "msg_a", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "hi"}}},
				StopReason: provider.StopEndTurn,
				Usage:      usage,
			},
		},
	}}

	var recorded []TurnMetrics
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		System:        []string{"base system prompt"},
		Now:           clock.now,
		OnTurnMetrics: func(m TurnMetrics) { recorded = append(recorded, m) },
	})

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt err = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("OnTurnMetrics calls = %d, want 1", len(recorded))
	}
	m := recorded[0]

	if m.SessionID != s.ID {
		t.Errorf("SessionID = %q, want %q", m.SessionID, s.ID)
	}
	if want := "test/m1"; m.Model.String() != want {
		t.Errorf("Model = %q, want %q", m.Model.String(), want)
	}
	if m.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", m.Attempt)
	}
	if m.TTFTMillis != 120 {
		t.Errorf("TTFTMillis = %d, want 120", m.TTFTMillis)
	}
	if m.StreamMillis != 380 {
		t.Errorf("StreamMillis = %d, want 380", m.StreamMillis)
	}
	if m.InputTokens != usage.InputTokens || m.OutputTokens != usage.OutputTokens ||
		m.CacheReadTokens != usage.CacheReadTokens || m.CacheWriteTokens != usage.CacheWriteTokens {
		t.Errorf("usage fields = %+v, want the provider.Usage fields passed through verbatim: %+v", m, usage)
	}

	// SystemLen/ToolsCount must match the exact request the provider
	// received — the join key against request.meta — not a hardcoded guess
	// about what ambient segments this session assembles.
	if len(prov.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(prov.requests))
	}
	wantSystemLen := len(strings.Join(prov.requests[0].System, "\n"))
	if m.SystemLen != wantSystemLen {
		t.Errorf("SystemLen = %d, want %d (len of the joined system the provider actually received)", m.SystemLen, wantSystemLen)
	}
	wantToolsCount := len(prov.requests[0].Tools)
	if m.ToolsCount != wantToolsCount {
		t.Errorf("ToolsCount = %d, want %d", m.ToolsCount, wantToolsCount)
	}
}

// TestTurnMetricsFirstDeltaLatches is the red-first guard for streamTurn's
// firstDeltaAt gate (engine.go): two activity events (a keep-alive ping, an
// in-progress tool-argument chunk — see provider.EventActivity's doc
// comment) precede the real content, and a SECOND text delta follows the
// first — TTFTMillis must land on the FIRST non-activity delta and never
// move again, including when a later delta arrives.
//
// This is deliberately checked by clock CALL COUNT, not just the final
// value: with a purely sequential fake clock, a bug that fires the gate on
// the wrong EVENT (say, the first activity instead of the first real delta)
// is unobservable in the output when the gate still fires exactly once —
// the Nth Now() call returns the same scripted instant regardless of which
// loop iteration asked for it. A bug that fires the gate MORE than once
// (forgetting the latch) is observable: it consumes an extra clock entry, so
// EventDone's own call is pushed onto a later, wrong-value slot. That is the
// mechanism this test actually red-verifies.
//
// Red-verify the NAMED mechanism: dropping the "!gotFirstDelta &&" half of
// streamTurn's gate (keeping only the EventActivity type check) makes the
// SECOND text delta re-fire the assignment — and, since EventDone itself is
// also != EventActivity, EventDone's own pre-switch pass re-fires it a
// THIRD time, stealing the value doneAt should get. Verified against that
// exact revert: TTFTMillis read 90 (not 50), StreamMillis read 410 (not
// 30), and clock.i read 5 (not 3) — every assertion below caught it.
func TestTurnMetricsFirstDeltaLatches(t *testing.T) {
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := &stepClock{times: []time.Time{
		base,                             // sentAt
		base.Add(50 * time.Millisecond),  // firstDeltaAt: the FIRST text delta
		base.Add(80 * time.Millisecond),  // doneAt (correct code stops here)
		base.Add(90 * time.Millisecond),  // never reached by correct code
		base.Add(500 * time.Millisecond), // never reached by correct code
	}}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		{
			{Type: provider.EventActivity},
			{Type: provider.EventActivity},
			{Type: provider.EventTextDelta, Text: "hi"},
			{Type: provider.EventTextDelta, Text: " there"},
			{
				Type:       provider.EventDone,
				Message:    &message.Message{ID: "msg_a", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "hi there"}}},
				StopReason: provider.StopEndTurn,
			},
		},
	}}
	var recorded []TurnMetrics
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		Now:           clock.now,
		OnTurnMetrics: func(m TurnMetrics) { recorded = append(recorded, m) },
	})

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt err = %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("OnTurnMetrics calls = %d, want 1", len(recorded))
	}
	// Exactly 3 of the clock's 4 scripted instants are consumed — sentAt,
	// the FIRST text delta, EventDone — proving neither the activity events
	// nor the second text delta ever called Now().
	if clock.i != 3 {
		t.Errorf("clock calls = %d, want 3", clock.i)
	}
	if recorded[0].TTFTMillis != 50 {
		t.Errorf("TTFTMillis = %d, want 50 (the first delta, not a later one)", recorded[0].TTFTMillis)
	}
	if recorded[0].StreamMillis != 30 {
		t.Errorf("StreamMillis = %d, want 30", recorded[0].StreamMillis)
	}
}

// TestTurnMetricsOnlyEmitsOnCompletedCall proves a turn that never reaches
// EventDone (a Stream dial error) emits no turn_metrics line at all — the
// deliverable is one line per COMPLETED model call, not one per attempt.
func TestTurnMetricsOnlyEmitsOnCompletedCall(t *testing.T) {
	prov := &flakyProvider{name: "test", failN: 100, err: retryableServerErr()}
	var recorded []TurnMetrics
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		OnTurnMetrics: func(m TurnMetrics) { recorded = append(recorded, m) },
	})

	if _, err := s.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("Prompt err = nil, want the permanent Stream failure to surface")
	}
	if len(recorded) != 0 {
		t.Errorf("OnTurnMetrics calls = %d, want 0 (no completed call ever happened)", len(recorded))
	}
}

// TestTurnMetricsRecordsRetryAttempt is the red-first guard for
// streamTurnWithRetry's attempt number reaching the emitted TurnMetrics.
// Attempt 1 fails retryably (a Stream dial error, so streamTurn never enters
// its event loop and never emits metrics for that attempt — see
// TestTurnMetricsOnlyEmitsOnCompletedCall); attempt 2 succeeds and must
// report Attempt == 2, not 1.
//
// Red-verify the NAMED mechanism: with streamTurn's TurnMetrics literal
// hardcoded to Attempt: 1 (dropping the attempt parameter), this test's
// m.Attempt != 2 assertion is the only one that fails — TestPromptRetries*
// still pass because none of them inspect TurnMetrics.
func TestTurnMetricsRecordsRetryAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
		clock := &stepClock{times: []time.Time{
			base,                              // attempt 1 sentAt (Stream fails before any event)
			base.Add(1 * time.Second),         // attempt 2 sentAt
			base.Add(1200 * time.Millisecond), // attempt 2 firstDeltaAt == doneAt (EventDone is the only event)
			base.Add(1200 * time.Millisecond), // attempt 2 doneAt
		}}
		prov := &flakyProvider{
			name:  "test",
			failN: 1,
			err:   retryableServerErr(),
			ok:    asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
		}
		var recorded []TurnMetrics
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			PromptRetries: 2,
			Now:           clock.now,
			OnTurnMetrics: func(m TurnMetrics) { recorded = append(recorded, m) },
		})

		if _, err := s.Prompt(context.Background(), "go"); err != nil {
			t.Fatalf("Prompt err = %v, want nil (the retry masks the blip)", err)
		}
		if len(recorded) != 1 {
			t.Fatalf("OnTurnMetrics calls = %d, want 1 (only the successful attempt completes)", len(recorded))
		}
		if recorded[0].Attempt != 2 {
			t.Errorf("Attempt = %d, want 2 (attempt 1 failed and was retried)", recorded[0].Attempt)
		}
		if recorded[0].TTFTMillis != 200 {
			t.Errorf("TTFTMillis = %d, want 200", recorded[0].TTFTMillis)
		}
	})
}

// TestDefaultTurnMetricsLogDoesNotPanic is a minimal smoke test for
// Config.OnTurnMetrics's default (see emitTurnMetrics/defaultTurnMetricsLog,
// turn_metrics.go): a session built with no OnTurnMetrics callback must
// still complete a turn without panicking, proving the stderr slog default
// is actually wired rather than left nil.
func TestDefaultTurnMetricsLogDoesNotPanic(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt err = %v", err)
	}
}

func TestTurnMetricsReportsCodexIncrementalProjection(t *testing.T) {
	const responseID = "resp_must_not_escape"
	metadata := &provider.RequestMetadata{
		Mode:                 provider.RequestModeIncremental,
		CompleteInputItems:   7,
		SentInputItems:       2,
		PreviousResponseUsed: true,
		ChainRecovered:       true,
	}
	prov := &scriptedProvider{name: "codex", turns: [][]provider.Event{{
		{Type: provider.EventDone, Message: &message.Message{ID: responseID, Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}}, StopReason: provider.StopEndTurn, RequestMetadata: metadata},
	}}}
	var recorded []TurnMetrics
	s := NewSession(Config{
		Providers:     provider.Registry{"codex": prov},
		Model:         message.ModelRef{Provider: "codex", Model: "gpt-5"},
		OnTurnMetrics: func(m TurnMetrics) { recorded = append(recorded, m) },
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("metrics records = %d, want 1", len(recorded))
	}
	got := recorded[0]
	if got.RequestMode != provider.RequestModeIncremental || got.CompleteInputItems != 7 || got.SentInputItems != 2 || !got.PreviousResponseUsed || !got.ChainRecovered {
		t.Fatalf("projection metrics = %+v, want incremental 7 complete, 2 sent, previous response used, chain recovered", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), responseID) {
		t.Fatalf("serialized metrics leaked response ID: %s", raw)
	}

	var log bytes.Buffer
	oldLogger := defaultTurnMetricsStderr
	defaultTurnMetricsStderr = slog.New(slog.NewJSONHandler(&log, nil))
	t.Cleanup(func() { defaultTurnMetricsStderr = oldLogger })
	defaultTurnMetricsLog(got)
	record := log.String()
	for _, field := range []string{
		`"request_mode":"incremental"`,
		`"complete_input_items":7`,
		`"sent_input_items":2`,
		`"previous_response_used":true`,
		`"chain_recovered":true`,
	} {
		if !strings.Contains(record, field) {
			t.Errorf("serialized turn_metrics record %q does not contain %s", record, field)
		}
	}
	if strings.Contains(record, responseID) {
		t.Fatalf("serialized turn_metrics record leaked response ID: %s", record)
	}
}
