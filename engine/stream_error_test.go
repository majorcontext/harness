package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// errAfterDeltaStream emits one text delta and then fails: it models a
// provider connection that drops mid-stream, after some tokens have already
// arrived.
type errAfterDeltaStream struct {
	text string
	err  error
	i    int
}

func (s *errAfterDeltaStream) Next() (provider.Event, error) {
	s.i++
	if s.i == 1 {
		return provider.Event{Type: provider.EventTextDelta, Text: s.text}, nil
	}
	if s.err != nil {
		return provider.Event{}, s.err
	}
	// No configured error: complete the turn normally (used to prove a
	// later Prompt on the same session still works after a prior failure).
	msg := &message.Message{ID: "msg_recovered", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: s.text}}}
	return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
}

func (s *errAfterDeltaStream) Close() error { return nil }

// midStreamErrProvider always returns a fresh errAfterDeltaStream.
type midStreamErrProvider struct {
	name string
	text string
	err  error
}

func (p *midStreamErrProvider) Name() string { return p.name }

func (p *midStreamErrProvider) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	return &errAfterDeltaStream{text: p.text, err: p.err}, nil
}

// TestPromptMidStreamProviderFailure drives a real Prompt through a scripted
// stream that emits a text delta and then fails on the next Next() call —
// e.g. a dropped connection partway through a response. It asserts:
//  1. the delta that did arrive reached OnEvent before the failure,
//  2. the stream error surfaces from Prompt unchanged,
//  3. the session is left in a sane state afterward: only the user message
//     appended before streaming started is in history — no partial or
//     poisoned assistant message — no EventMessage was emitted, and the
//     on-disk log is still loadable and agrees with in-memory history.
func TestPromptMidStreamProviderFailure(t *testing.T) {
	dir := t.TempDir()
	streamErr := errors.New("fake connection reset mid-stream")
	prov := &midStreamErrProvider{name: "test", text: "partial resp", err: streamErr}

	var events []Event
	cfg := Config{
		Providers:  provider.Registry{prov.name: prov},
		Model:      message.ModelRef{Provider: prov.name, Model: "m1"},
		SessionDir: dir,
		OnEvent:    func(ev Event) { events = append(events, ev) },
	}
	s := NewSession(cfg)

	msg, err := s.Prompt(context.Background(), "hello")
	if msg != nil {
		t.Errorf("Prompt returned a message on error: %+v", msg)
	}
	if !errors.Is(err, streamErr) {
		t.Fatalf("Prompt err = %v, want %v", err, streamErr)
	}

	// The delta reached OnEvent before the error, and no message event was
	// ever emitted (streamTurn returned before Prompt could append/emit the
	// assistant message).
	var deltas []string
	for _, ev := range events {
		switch ev.Type {
		case EventTextDelta:
			deltas = append(deltas, ev.Text)
		case EventMessage:
			t.Errorf("unexpected message event on a failed turn: %+v", ev)
		}
	}
	if len(deltas) != 1 || deltas[0] != "partial resp" {
		t.Errorf("text deltas = %v, want [%q]", deltas, "partial resp")
	}

	// History holds only the user message appended before streaming: no
	// partial/poisoned assistant message.
	h := s.History()
	if len(h) != 1 {
		t.Fatalf("history len = %d, want 1: %+v", len(h), h)
	}
	if h[0].Role != message.RoleUser || h[0].Parts.Text() != "hello" {
		t.Fatalf("history[0] = %+v, want user %q", h[0], "hello")
	}

	if perr := s.PersistErr(); perr != nil {
		t.Fatalf("PersistErr = %v", perr)
	}

	// The session log is still loadable and agrees with in-memory history —
	// the failed turn left no half-written record behind.
	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	lh := loaded.History()
	if len(lh) != 1 {
		t.Fatalf("loaded history len = %d, want 1: %+v", len(lh), lh)
	}
	if lh[0].Role != message.RoleUser || lh[0].Parts.Text() != "hello" {
		t.Fatalf("loaded history[0] = %+v, want user %q", lh[0], "hello")
	}

	// The session is not wedged: a subsequent Prompt against the same
	// session, once the provider recovers, succeeds normally.
	prov.err = nil
	final, err := s.Prompt(context.Background(), "again")
	if err != nil {
		t.Fatalf("second Prompt after recovery: %v", err)
	}
	if final.Parts.Text() != "partial resp" {
		t.Errorf("second Prompt final = %q", final.Parts.Text())
	}
}
