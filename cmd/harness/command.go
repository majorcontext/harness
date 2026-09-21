package main

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// runModeOps declares which control Ops `harness run` performs. A false
// value is an explicit refusal with a reason, not an omission: the
// support matrix must stay total, so a new Op cannot go silently
// unhandled. See docs/design/slash-commands.md §5.
var runModeOps = map[command.Op]bool{
	command.OpCompact:        true,
	command.OpSetModel:       true,
	command.OpSetThinking:    true,
	command.OpSetServiceTier: true,
	command.OpAbort:          false,
	command.OpSetGoal:        false,
	command.OpClearGoal:      false,
	command.OpQueueList:      false,
	command.OpQueueClear:     false,
	command.OpStatus:         false,
	command.OpProcessList:    false,
}

// resName names a resolution for an error message. Resolve always sets
// Spec, but an error path must not panic on a hand-built Resolution.
func resName(res command.Resolution) string {
	if res.Spec != nil {
		return res.Spec.Name
	}
	return string(res.Op)
}

// compactSkipMessage renders a CompactResult.SkipReason as a sentence a
// person at a terminal can act on. A skip is not success: run mode has no
// server response to carry skip_reason silently, so the reason must reach
// the user through the command's own error text instead (§5).
func compactSkipMessage(reason string) string {
	switch reason {
	case engine.SkipReasonNotEnoughTurns:
		return "the session does not have enough turns yet to fold"
	case engine.SkipReasonLoneExistingSummary:
		return "the session's history is already a single summary with nothing left to fold"
	case engine.SkipReasonSummarizerEmpty:
		return "the summarizer returned no usable summary"
	default:
		return reason
	}
}

// dispatchCommand performs one resolved control command against the
// session a run holds. Session resolution is not a concern here: a run
// has exactly one session, and it is a root.
func dispatchCommand(ctx context.Context, s *engine.Session, res command.Resolution) error {
	if res.Kind == command.KindFrontend {
		return fmt.Errorf("/%s is a frontend command; harness run does not own the session pointer", resName(res))
	}
	if supported, known := runModeOps[res.Op]; !known || !supported {
		return fmt.Errorf("/%s is not available in this mode: harness run has no server to perform %q", resName(res), res.Op)
	}
	switch res.Op {
	case command.OpCompact:
		var opts engine.CompactOptions
		if n, ok := res.Args["keep_turns"].(int); ok {
			opts.KeepTurns = n
		}
		result, err := s.Compact(ctx, opts)
		if err != nil {
			return err
		}
		if result.SkipReason != "" {
			return fmt.Errorf("/compact did nothing: %s", compactSkipMessage(result.SkipReason))
		}
		return nil
	case command.OpSetModel:
		ref, err := message.ParseModelRef(res.Args["model"].(string))
		if err != nil {
			return err
		}
		if !s.ModelSupported(ref) {
			return fmt.Errorf("provider %q is not configured", ref.Provider)
		}
		if err := s.CheckModel(ref); err != nil {
			return err
		}
		s.SetModel(ref)
		return nil
	case command.OpSetThinking:
		e, err := message.ParseEffort(res.Args["effort"].(string))
		if err != nil {
			return err
		}
		s.SetEffort(e)
		return nil
	case command.OpSetServiceTier:
		s.SetServiceTier(res.Args["service_tier"].(string))
		return nil
	}
	return fmt.Errorf("unhandled op %q", res.Op)
}
