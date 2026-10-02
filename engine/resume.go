package engine

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
)

const resumedToolCallErrorText = "The process restarted while this tool call was running; its outcome is unknown."

const delegatedResumeText = "Your previous turn was interrupted by a restart. Continue from where you left off."

// ErrNotResumable is returned by ResumeTurn when no turn may resume.
var ErrNotResumable = errors.New("engine: no resumable turn")

// ErrTurnStopped is the context cancel cause of a deliberate stop. A turn
// canceled with this cause keeps its partial reply and is never resumed.
var ErrTurnStopped = errors.New("engine: turn stopped")

// handoffCanceled reports whether ctx was canceled for a handoff rather than
// a stop, on a root that may resume. The caller must then leave the tool
// calls unresolved: a journaled canceled result would hide a call that may
// have run, and the next holder would never re-run it.
func (s *Session) handoffCanceled(ctx context.Context) bool {
	return s.cfg.MaxTurnResumes > 0 && !s.hasTaskParent() &&
		errors.Is(ctx.Err(), context.Canceled) &&
		!errors.Is(context.Cause(ctx), ErrTurnStopped)
}

// PartialMessageID returns the id of the partial assistant message that a
// stopped turn appended before it failed with err, or "" when err names none.
func PartialMessageID(err error) string {
	var interrupted *interruptedTurnError
	if errors.As(err, &interrupted) && interrupted.partial != nil {
		return interrupted.partial.ID
	}
	return ""
}

// TurnStopped reports whether the journal ends in a turn.stopped record.
func (s *Session) TurnStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnStopped
}

// TurnUnfinished reports whether a turn started and never settled: the
// journal holds messages of a turn that no outcome closed.
func (s *Session) TurnUnfinished() bool { return s.hasUnfinalizedTurn() }

// ResumeCapReached reports whether the turn has used all the resumes that
// Config.MaxTurnResumes allows. It is true when MaxTurnResumes is zero.
func (s *Session) ResumeCapReached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnResumes >= s.cfg.MaxTurnResumes
}

// RecordTurnStopped durably settles the current turn as stopped. The turn
// never resumes. partialMessageID names the partial assistant message the
// stop kept, or is empty when the stop came before any reply text.
func (s *Session) RecordTurnStopped(partialMessageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.ensureLog(); err != nil {
			s.lastPersistErr = err
			return err
		}
		s.flushQueueRecordsLocked()
		if err := s.writeRecord(record{Type: recTurnStopped, MessageID: partialMessageID}); err != nil {
			s.lastPersistErr = err
			return err
		}
		if !s.volumeSync() {
			if err := s.store.Sync(s.ID); err != nil {
				s.lastPersistErr = err
				return err
			}
		}
	}
	s.turnUnsettled = false
	s.turnResumes = 0
	s.turnStopped = true
	return nil
}

// ResumableTurn reports whether ResumeTurn may run: a root session with an
// unsettled turn, resumes left under Config.MaxTurnResumes, no committed
// outcome, and a journal that does not already end in a final answer.
func (s *Session) ResumableTurn() bool {
	if s.hasTaskParent() {
		return false
	}
	s.mu.Lock()
	ok := s.turnUnsettled && s.turnResumes < s.cfg.MaxTurnResumes && s.committedOutcome == nil
	s.mu.Unlock()
	if !ok {
		return false
	}
	_, done := s.settledSuccessResult()
	return !done
}

func (s *Session) TurnResumes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnResumes
}

// ResumeTurn continues a turn that a crash left unsettled. It counts the
// resume in the journal before any model call, so a turn that crashes the
// process every time still reaches Config.MaxTurnResumes. The partial model
// response of the crashed call is not in the journal; the model call is
// issued again from the last journaled message.
func (s *Session) ResumeTurn(ctx context.Context) (*message.Message, error) {
	if !s.ResumableTurn() {
		return nil, ErrNotResumable
	}
	if err := s.recordTurnResume(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	if backend, ok := s.delegatedBackend(); ok {
		if s.trailingMessage().Role == message.RoleUser {
			return s.runDelegatedTurn(ctx, backend)
		}
		return s.PromptEngineResume(ctx, delegatedResumeText)
	}
	if err := s.ConfigErr(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	if err := s.ContextWindowErr(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	if err := s.ensureInstructions(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	if err := s.ensureSkills(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	if trailing := s.dropLoadRepair(); trailing.Role == message.RoleAssistant && hasToolCall(trailing) {
		res := s.resumeToolResults(ctx, &trailing)
		if s.handoffCanceled(ctx) {
			return nil, ctx.Err()
		}
		s.append(res)
	}
	return s.runAgenticLoop(ctx)
}

func (s *Session) recordTurnResume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.turnResumes + 1
	if s.store != nil {
		if err := s.ensureLog(); err != nil {
			s.lastPersistErr = err
			return err
		}
		s.flushQueueRecordsLocked()
		if err := s.writeRecord(record{Type: recTurnResumed, Count: n}); err != nil {
			s.lastPersistErr = err
			return err
		}
		if !s.volumeSync() {
			if err := s.store.Sync(s.ID); err != nil {
				s.lastPersistErr = err
				return err
			}
		}
	}
	s.turnResumes = n
	s.emit(Event{Type: EventTurnResumed, Text: strconv.Itoa(n)})
	return nil
}

func (s *Session) trailingMessage() message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return message.Message{}
	}
	return s.history[len(s.history)-1]
}

// dropLoadRepair removes the memory-only tool-result message that
// LoadSession synthesizes for unresolved tool calls at the journal tail, and
// returns the trailing message that remains. ResumeTurn then journals real
// results in its place.
func (s *Session) dropLoadRepair() message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.history)
	if n > 1 && s.history[n-1].Role == message.RoleTool && strings.HasPrefix(s.history[n-1].ID, message.SyntheticOrphanIDPrefix) {
		s.history = s.history[:n-1]
		n--
	}
	if n == 0 {
		return message.Message{}
	}
	return s.history[n-1]
}

func hasToolCall(m message.Message) bool {
	for _, p := range m.Parts {
		if _, ok := p.(*message.ToolCall); ok {
			return true
		}
	}
	return false
}

func (s *Session) resumeToolResults(ctx context.Context, asst *message.Message) message.Message {
	if !s.cfg.ResumeRerunTools {
		return syntheticUnexecutedToolResults(asst, resumedToolCallErrorText)
	}
	return message.Message{
		ID:        newID("msg"),
		Role:      message.RoleTool,
		Parts:     s.runToolCalls(ctx, asst),
		CreatedAt: time.Now().UTC(),
	}
}
