package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type textThenBlockProvider struct {
	name    string
	text    string
	emitted chan struct{}
}

func (p *textThenBlockProvider) Name() string { return p.name }

func (p *textThenBlockProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	return &textThenBlockStream{ctx: ctx, p: p}, nil
}

type textThenBlockStream struct {
	ctx  context.Context
	p    *textThenBlockProvider
	sent bool
}

func (s *textThenBlockStream) Next() (provider.Event, error) {
	if !s.sent && s.p.text != "" {
		s.sent = true
		close(s.p.emitted)
		return provider.Event{Type: provider.EventTextDelta, Text: s.p.text, ID: "msg_partial"}, nil
	}
	<-s.ctx.Done()
	return provider.Event{}, s.ctx.Err()
}

func (s *textThenBlockStream) Close() error { return nil }

func stopAfterText(t *testing.T, text string, cause error) (SessionStore, Config, *Session, error) {
	t.Helper()
	st := NewMemStore()
	prov := &textThenBlockProvider{name: "p", text: text, emitted: make(chan struct{})}
	cfg := resumeConfig(st, prov, 3)
	s := NewSession(cfg)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(ctx, "q")
		done <- err
	}()
	if text != "" {
		<-prov.emitted
	}
	cancel(cause)
	return st, cfg, s, <-done
}

func TestStopKeepsPartialTextAndSettlesTurn(t *testing.T) {
	st, cfg, s, err := stopAfterText(t, "par", ErrTurnStopped)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt error = %v, want context.Canceled", err)
	}
	partialID := PartialMessageID(err)
	if partialID != "msg_partial" {
		t.Fatalf("PartialMessageID = %q, want msg_partial", partialID)
	}
	if err := s.RecordTurnStopped(partialID); err != nil {
		t.Fatalf("RecordTurnStopped: %v", err)
	}

	recs := journalOf(t, st, s.ID)
	tail := recs[len(recs)-2:]
	if tail[0].Type != recMessage || tail[0].Message.ID != "msg_partial" || tail[0].Message.Parts.Text() != "par" {
		t.Errorf("record before the marker = %+v, want the partial assistant message", tail[0])
	}
	if tail[1].Type != recTurnStopped || tail[1].MessageID != "msg_partial" {
		t.Errorf("last record = %+v, want turn.stopped naming msg_partial", tail[1])
	}

	reloaded := reloadSession(t, st, cfg, s.ID)
	if reloaded.ResumableTurn() {
		t.Error("ResumableTurn() = true after a stop, want false")
	}
	if !reloaded.TurnStopped() {
		t.Error("TurnStopped() = false after reload, want true")
	}
	reloaded.append(userMsg("u2", "next"))
	if reloaded.TurnStopped() {
		t.Error("TurnStopped() = true after a later append, want false")
	}
}

func TestStopBeforeTextWritesMarkerWithoutMessageID(t *testing.T) {
	st, cfg, s, err := stopAfterText(t, "", ErrTurnStopped)
	if got := PartialMessageID(err); got != "" {
		t.Fatalf("PartialMessageID = %q, want empty", got)
	}
	if err := s.RecordTurnStopped(""); err != nil {
		t.Fatalf("RecordTurnStopped: %v", err)
	}
	recs := journalOf(t, st, s.ID)
	last := recs[len(recs)-1]
	if last.Type != recTurnStopped || last.MessageID != "" {
		t.Errorf("last record = %+v, want turn.stopped with no message_id", last)
	}
	if reloadSession(t, st, cfg, s.ID).ResumableTurn() {
		t.Error("ResumableTurn() = true after a stop, want false")
	}
}

func TestPlainCancelKeepsTurnResumableAndSavesNoPartial(t *testing.T) {
	st, cfg, s, err := stopAfterText(t, "par", context.Canceled)
	if got := PartialMessageID(err); got != "" {
		t.Fatalf("PartialMessageID = %q, want empty for a cancel that is not a stop", got)
	}
	recs := journalOf(t, st, s.ID)
	if last := recs[len(recs)-1]; last.Type != recMessage || last.Message.Role != message.RoleUser {
		t.Errorf("last record = %+v, want only the user message", last)
	}
	if !reloadSession(t, st, cfg, s.ID).ResumableTurn() {
		t.Error("ResumableTurn() = false after a plain cancel, want true")
	}
}

func TestClearedTurnKeepsNoPartialText(t *testing.T) {
	st, _, s, err := stopAfterText(t, "par", ErrTurnCleared)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt error = %v, want context.Canceled", err)
	}
	if got := PartialMessageID(err); got != "" {
		t.Fatalf("PartialMessageID = %q, want empty for a cleared turn", got)
	}
	recs := journalOf(t, st, s.ID)
	if last := recs[len(recs)-1]; last.Type != recMessage || last.Message.Role != message.RoleUser {
		t.Errorf("last record = %+v, want only the user message", last)
	}
	if errors.Is(ErrTurnCleared, ErrTurnStopped) {
		t.Error("ErrTurnCleared wraps ErrTurnStopped, want a distinct cause")
	}
}

func TestClearedTurnStillRecordsToolResults(t *testing.T) {
	st := NewMemStore()
	bt := &blockingCallTool{started: make(chan struct{}), block: true}
	prov := scriptedTurns("p", [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("t1", "probe", `{}`)),
	}).(*scriptedProvider)
	cfg := resumeConfig(st, prov, 3, bt.tool())
	s := NewSession(withStore(cfg, st))

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(ctx, "q")
		done <- err
	}()
	select {
	case <-bt.started:
	case err := <-done:
		t.Fatalf("Prompt returned before the tool started: %v", err)
	}
	cancel(ErrTurnCleared)
	<-done

	got := realToolResults(reloadSession(t, st, cfg, s.ID).History())
	if len(got) != 1 {
		t.Fatalf("tool-result messages = %+v, want the clear to record one", got)
	}
	if res, ok := got[0].Parts[0].(*message.ToolResult); !ok || res.CallID != "t1" {
		t.Errorf("recorded result = %+v, want a result for t1", got[0].Parts[0])
	}
}
