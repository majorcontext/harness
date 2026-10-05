package main

import (
	"errors"
	"fmt"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/message"
)

const claudeCodeProvider = "claude-code"

// runModeOps lists the control commands that `harness run` performs. Every
// other command is refused.
var runModeOps = map[command.Op]bool{
	command.OpCompact:        true,
	command.OpSetModel:       true,
	command.OpSetThinking:    true,
	command.OpSetServiceTier: true,
}

// unknownLine is a /name that no built-in command and no prompt command of
// the working directory owns. A session of the Claude Code CLI may still
// answer it.
type unknownLine struct{ err error }

// refuseOn refuses the line when model is not a Claude Code model.
func (u *unknownLine) refuseOn(model string) error {
	if ref, err := message.ParseModelRef(model); err == nil && ref.Provider == claudeCodeProvider {
		return nil
	}
	return u.err
}

// checkLine refuses a run before it creates a session: a goal with no
// evaluator model, a command with surplus arguments, a frontend command, a
// command that run does not perform, a control command with no session to
// run on, and an unknown /name on a model that is not Claude Code. It returns
// the unknown /name when the model of the run is the one of the opened
// session, and so is not known yet.
func checkLine(cfg *config.Config, rt *harness.Runtime, opts runOptions, modelSet bool) (*unknownLine, error) {
	if opts.goal != "" {
		if cfg.GoalEvaluatorModel == "" {
			return nil, errors.New("goal_evaluator_model must be set in config to use -goal")
		}
		return nil, nil
	}
	res, err := command.NewRegistry().Resolve(opts.prompt)
	unknown, isUnknown := errors.AsType[*command.UnknownCommandError](err)
	switch {
	case err == nil:
		if err := checkRunModeSupport(res); err != nil {
			return nil, err
		}
		if opts.resume == "" && !opts.cont {
			return nil, fmt.Errorf("/%s needs an existing session: pass -resume or -continue, or drop the command and send a plain prompt", res.Spec.Name)
		}
		return nil, nil
	case errors.Is(err, command.ErrNotCommand):
		return nil, nil
	case !isUnknown:
		return nil, err
	}
	owned, err := promptCommand(rt, unknown.Name)
	if err != nil || owned {
		return nil, err
	}
	line := &unknownLine{err: unknown}
	if opts.resume == "" && !opts.cont || modelSet {
		return nil, line.refuseOn(cfg.ResolveModel(opts.model))
	}
	return line, nil
}

func promptCommand(rt *harness.Runtime, name string) (bool, error) {
	menu, err := rt.Commands()
	if err != nil {
		return false, err
	}
	for _, e := range menu.Commands {
		if e.Name == name && e.Kind == string(command.KindPrompt) {
			return true, nil
		}
	}
	return false, nil
}

// checkRunModeSupport refuses a command that run cannot perform.
func checkRunModeSupport(res command.Resolution) error {
	if res.Kind == command.KindFrontend {
		return fmt.Errorf("/%s is a frontend command; harness run does not own the session pointer", res.Spec.Name)
	}
	if !runModeOps[res.Op] {
		return fmt.Errorf("/%s is not available in this mode: harness run has no server to perform %q", res.Spec.Name, res.Op)
	}
	return nil
}
