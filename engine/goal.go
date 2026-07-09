// Goal loop: pursue a completion condition with an independent evaluator.
//
// PursueGoal drives the ordinary Prompt loop toward a natural-language
// condition. After every turn it asks a second, TOOL-LESS model — the
// evaluator, resolved through the same provider registry — whether the
// condition is met, feeding the evaluator's reason back as guidance for the
// next turn until the condition is met or the turn budget runs out.
//
// This is a plan-artifact-free, gate-free loop: it introduces no plan mode and
// no permission gate (see AGENTS.md, "Deliberately absent"). It is a control
// loop over Prompt plus a read-only evaluator call, nothing more.
//
// Durable goal.* records land in the session log so a resumed session can tell
// whether a goal is still active (see store.go, ActiveGoal). The loop also
// emits goal.* engine events so a server can journal them.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// GoalOptions configures a PursueGoal run.
type GoalOptions struct {
	// Registered indicates the caller already called RegisterGoal
	// synchronously; PursueGoal then treats an inactive goal at loop start
	// as cleared-before-start rather than registering a fresh one.
	Registered bool

	// MaxTurns caps the number of worker turns; 0 means unlimited.
	MaxTurns int
	// Evaluator is the model ref used for the completion check. It is required
	// — the engine hardcodes no default — and is resolved through the same
	// provider registry as the worker model.
	Evaluator message.ModelRef
}

// GoalResult is the outcome of a PursueGoal run.
type GoalResult struct {
	Achieved bool
	Turns    int
	Reason   string
}

// goalEvaluatorSystem instructs the evaluator to answer in a strict two-form
// vocabulary. Parsing is lenient (prefix, case-insensitive) so a stray period
// or lowercase reply still resolves.
const goalEvaluatorSystem = `You are a strict goal-completion evaluator for an autonomous agent.
You are given a GOAL CONDITION and a transcript of the agent's work so far.
Decide whether the condition has been FULLY satisfied by the work shown.

Reply with EXACTLY ONE line, in one of these two forms and nothing else:
MET: <one short sentence saying why>
NOT MET: <one short sentence saying what is still missing>

Do not add any other text, headings, markdown, or code fences.`

// goalPartCap bounds each rendered transcript part so a long tool result cannot
// blow up the evaluator request.
const goalPartCap = 4096

// errEvaluatorUnparseable is returned when two consecutive evaluator replies
// cannot be parsed — the loop errors rather than spinning.
var errEvaluatorUnparseable = errors.New("engine: goal evaluator returned unparseable output twice in a row")

// PursueGoal runs the goal loop: prompt the condition, then after every turn
// ask the evaluator whether it is met, feeding the evaluator's reason back as
// guidance until the condition is met or MaxTurns is exhausted.
//
// Turn 1 prompts the raw condition as the directive. A NOT MET verdict makes
// the next directive a fixed-template guidance message carrying the evaluator's
// reason. Returns Achieved=true on the first MET verdict; Achieved=false with
// reason "max turns" when the budget runs out. A cancelled context, a provider
// error, or two unparseable evaluator replies in a row return an error.
//
// Must not be called concurrently with itself or Prompt (it drives Prompt).
func (s *Session) PursueGoal(ctx context.Context, condition string, opts GoalOptions) (*GoalResult, error) {
	if opts.Evaluator.IsZero() {
		return nil, errors.New("engine: PursueGoal requires GoalOptions.Evaluator")
	}
	if strings.TrimSpace(condition) == "" {
		return nil, errors.New("engine: PursueGoal requires a non-empty condition")
	}

	if opts.Registered {
		// The accepting caller registered synchronously (the server handler
		// does, closing the accept-vs-clear race). If the goal is no longer
		// active, a clear won the race before the loop started: clean stop.
		if !s.goalActiveWith(condition) {
			return &GoalResult{Achieved: false, Turns: 0, Reason: "goal cleared"}, nil
		}
	} else if err := s.RegisterGoal(condition); err != nil {
		return nil, err
	}

	directive := condition
	for turn := 1; opts.MaxTurns == 0 || turn <= opts.MaxTurns; turn++ {
		if !s.goalActiveWith(condition) {
			// Cleared between registration and this turn (or mid-loop by a
			// concurrent DELETE): clean stop, no turn runs.
			return &GoalResult{Achieved: false, Turns: turn - 1, Reason: "goal cleared"}, nil
		}
		if _, err := s.Prompt(ctx, directive); err != nil {
			return nil, err
		}
		met, reason, err := s.evaluateGoal(ctx, condition, opts.Evaluator)
		if err != nil {
			return nil, err
		}
		if !s.recordGoalEval(met, reason, turn) {
			// ClearGoal fired while this evaluation was in flight: the goal is
			// no longer active, so its verdict must not land in the journal.
			// Treat this as a clean stop, never an achievement.
			return &GoalResult{Achieved: false, Turns: turn, Reason: "goal cleared"}, nil
		}
		if met {
			if !s.achieveGoal(reason, turn) {
				// Cleared in the narrow window between recordGoalEval and
				// achieveGoal — still a clean stop, not an achievement.
				return &GoalResult{Achieved: false, Turns: turn, Reason: "goal cleared"}, nil
			}
			return &GoalResult{Achieved: true, Turns: turn, Reason: reason}, nil
		}
		directive = goalGuidance(condition, reason)
	}
	return &GoalResult{Achieved: false, Turns: opts.MaxTurns, Reason: "max turns"}, nil
}

// ActiveGoal reports the current goal's condition when one is set but not yet
// achieved or cleared. On a resumed session it reflects the session log's
// goal.* records (condition only; run counters reset per Claude Code semantics).
// It never auto-runs a goal — the caller decides.
func (s *Session) ActiveGoal() (condition string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goalCondition, s.goalActive
}

// ClearGoal cancels an active goal: it writes a durable goal.cleared record,
// resets the in-memory goal state, and emits a goal.cleared event. It reports
// whether a goal was active (false is a no-op, so a repeated clear is
// idempotent).
//
// Ordering guarantee: ClearGoal journals and emits goal.cleared synchronously,
// under s.mu, before it returns. A caller that also needs to cancel the loop's
// context (e.g. the server's DELETE /goal handler) MUST call ClearGoal first
// and cancel second: cancelling first lets the goal-loop worker's
// context-cancellation unwind — which ends in a terminal status-idle record —
// race this call to the journal, so goal.cleared could be journaled after the
// idle record it is supposed to precede. Clear-then-cancel makes that
// structurally impossible: by the time cancellation can wake the worker,
// goal.cleared is already durable.
func (s *Session) ClearGoal() bool {
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return false
	}
	s.goalActive = false
	s.goalCondition = ""
	s.persistGoalLocked(recGoalCleared, goalRecord{})
	// Emit while still holding s.mu: this keeps the event stream (-> server
	// journal/SSE seqs) ordered the same as the log write above under a
	// concurrent recordGoalEval/achieveGoal race (see those functions).
	// OnEvent must not call back into this Session — doing so would
	// deadlock on s.mu, which is still held here.
	s.emit(Event{Type: EventGoalCleared})
	s.mu.Unlock()
	return true
}

// RegisterGoal records goal.set and marks the goal active. It is called
// synchronously by whoever accepts the goal (the HTTP handler, the CLI)
// BEFORE any loop goroutine spawns, so a ClearGoal arriving after acceptance
// always observes an active goal — the round-3 registration race is
// structurally impossible. Errors if a goal is already active.
func (s *Session) RegisterGoal(condition string) error {
	if strings.TrimSpace(condition) == "" {
		return errors.New("engine: RegisterGoal requires a non-empty condition")
	}
	s.mu.Lock()
	if s.goalActive {
		cur := s.goalCondition
		s.mu.Unlock()
		return fmt.Errorf("engine: a goal is already active: %q", cur)
	}
	s.goalActive = true
	s.goalCondition = condition
	s.persistGoalLocked(recGoalSet, goalRecord{Condition: condition})
	// Emit while holding s.mu (see ClearGoal): event order matches log
	// order. OnEvent must not call back into this Session.
	s.emit(Event{Type: EventGoalSet, GoalCondition: condition})
	s.mu.Unlock()
	return nil
}

// goalActiveWith reports whether the given condition is the currently
// active goal.
func (s *Session) goalActiveWith(condition string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goalActive && s.goalCondition == condition
}

// recordGoalEval records one evaluator verdict for a turn. It is a no-op —
// no journal write, no event — when the goal is no longer active: a
// concurrent ClearGoal may have raced this evaluation to completion, and its
// verdict must never land in the log after goal.cleared. Reports whether the
// record was written.
func (s *Session) recordGoalEval(met bool, reason string, turn int) bool {
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return false
	}
	s.persistGoalLocked(recGoalEval, goalRecord{Met: met, Reason: reason, Turn: turn})
	// Emit while still holding s.mu (see ClearGoal): keeps event order
	// matching log order under a concurrent clear. OnEvent must not call
	// back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventGoalEval, GoalMet: met, GoalReason: reason, GoalTurn: turn})
	s.mu.Unlock()
	return true
}

// achieveGoal records goal.achieved and clears the active goal. It is a
// no-op when the goal is no longer active (already cleared concurrently),
// so a cleared-then-achieved sequence can never reach the log. Reports
// whether the goal was achieved.
func (s *Session) achieveGoal(reason string, turns int) bool {
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return false
	}
	s.goalActive = false
	s.goalCondition = ""
	s.persistGoalLocked(recGoalAchieved, goalRecord{Reason: reason, Turns: turns})
	// Emit while still holding s.mu (see ClearGoal): keeps event order
	// matching log order under a concurrent clear. OnEvent must not call
	// back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventGoalAchieved, GoalReason: reason, GoalTurns: turns})
	s.mu.Unlock()
	return true
}

// evaluateGoal runs a single tool-less evaluator request and parses its
// verdict, retrying once on an unparseable reply — two unparseable replies in a
// row are an error (never silently spin). A provider or context error surfaces
// immediately.
func (s *Session) evaluateGoal(ctx context.Context, condition string, evaluator message.ModelRef) (met bool, reason string, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		out, err := s.runEvaluator(ctx, condition, evaluator)
		if err != nil {
			return false, "", err
		}
		if m, r, ok := parseEvaluation(out); ok {
			return m, r, nil
		}
	}
	return false, "", errEvaluatorUnparseable
}

// runEvaluator issues one tool-less completion check on the evaluator model and
// returns its raw text.
func (s *Session) runEvaluator(ctx context.Context, condition string, evaluator message.ModelRef) (string, error) {
	prov, err := s.cfg.Providers.For(evaluator)
	if err != nil {
		return "", err
	}
	content := "GOAL CONDITION:\n" + condition + "\n\nCONVERSATION TRANSCRIPT:\n" + renderConversation(s.History())
	req := &provider.Request{
		Model:  evaluator,
		System: []string{goalEvaluatorSystem},
		Messages: []message.Message{{
			ID:    newID("msg"),
			Role:  message.RoleUser,
			Parts: message.Parts{&message.Text{Text: content}},
		}},
		MaxTokens: 256,
	}
	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var deltas strings.Builder
	var doneText string
	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch ev.Type {
		case provider.EventTextDelta:
			deltas.WriteString(ev.Text)
		case provider.EventDone:
			if ev.Message != nil {
				doneText = ev.Message.Parts.Text()
			}
		}
	}
	if doneText != "" {
		return doneText, nil
	}
	return deltas.String(), nil
}

// parseEvaluation leniently reads a verdict: a case-insensitive "NOT MET" or
// "MET" prefix (checked NOT MET first, since it is not a MET prefix), with the
// remainder after an optional colon taken as the reason.
func parseEvaluation(out string) (met bool, reason string, ok bool) {
	t := strings.TrimSpace(out)
	up := strings.ToUpper(t)
	switch {
	case strings.HasPrefix(up, "NOT MET"):
		return false, trimReason(t[len("NOT MET"):]), true
	case strings.HasPrefix(up, "MET"):
		return true, trimReason(t[len("MET"):]), true
	default:
		return false, "", false
	}
}

func trimReason(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, ":")
	return strings.TrimSpace(s)
}

// goalGuidance is the fixed-template directive sent after a NOT MET verdict.
func goalGuidance(condition, reason string) string {
	return "The goal has not been met yet.\n\nGOAL: " + condition +
		"\n\nEVALUATOR FEEDBACK: " + reason +
		"\n\nKeep working until the goal is fully satisfied, then stop."
}

// renderConversation renders history compactly for the evaluator: each message
// role-labeled, each part rendered as text and capped at goalPartCap.
func renderConversation(history []message.Message) string {
	var b strings.Builder
	for _, m := range history {
		b.WriteString(strings.ToUpper(string(m.Role)))
		b.WriteString(":\n")
		for _, p := range m.Parts {
			b.WriteString(truncateForGoal(renderPart(p)))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func renderPart(p message.Part) string {
	switch v := p.(type) {
	case *message.Text:
		return v.Text
	case *message.Reasoning:
		return "[reasoning] " + v.Text
	case *message.ToolCall:
		return fmt.Sprintf("[tool call %s] %s", v.Name, string(v.Arguments))
	case *message.ToolResult:
		s := "[tool result] " + v.Content.Text()
		if v.IsError {
			s = "[tool result (error)] " + v.Content.Text()
		}
		return s
	case *message.Blob:
		return "[blob " + v.MediaType + "]"
	default:
		return ""
	}
}

func truncateForGoal(s string) string {
	if len(s) <= goalPartCap {
		return s
	}
	return s[:goalPartCap] + "…[truncated]"
}
