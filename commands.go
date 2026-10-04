package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/protocol"
)

// op runs the operation of a control command. method and path name the
// route of the same operation, or are empty.
type op struct {
	method, path string
	run          func(ctx context.Context, s *Session, args map[string]any) (any, error)
}

func update(p func(string) protocol.SettingsPatch, key string) func(context.Context, *Session, map[string]any) (any, error) {
	return func(ctx context.Context, s *Session, args map[string]any) (any, error) {
		return s.Update(ctx, p(args[key].(string)))
	}
}

var ops = map[command.Op]op{
	command.OpAbort: {"POST", "/sessions/{id}/interrupt", func(ctx context.Context, s *Session, _ map[string]any) (any, error) {
		return nil, s.Interrupt(ctx, protocol.Interrupt{})
	}},
	command.OpCompact: {"POST", "/sessions/{id}/compact", compact},
	command.OpSetGoal: {"PUT", "/sessions/{id}/goal", func(ctx context.Context, s *Session, args map[string]any) (any, error) {
		if err := s.SetGoal(ctx, protocol.Goal{Condition: args["condition"].(string)}); err != nil {
			return nil, err
		}
		return s.View(), nil
	}},
	command.OpClearGoal: {"DELETE", "/sessions/{id}/goal", func(ctx context.Context, s *Session, _ map[string]any) (any, error) {
		return nil, s.ClearGoal(ctx)
	}},
	command.OpSetModel:       {"PATCH", "/sessions/{id}", update(func(v string) protocol.SettingsPatch { return protocol.SettingsPatch{Model: &v} }, "model")},
	command.OpSetThinking:    {"PATCH", "/sessions/{id}", update(func(v string) protocol.SettingsPatch { return protocol.SettingsPatch{Effort: &v} }, "effort")},
	command.OpSetServiceTier: {"PATCH", "/sessions/{id}", update(func(v string) protocol.SettingsPatch { return protocol.SettingsPatch{ServiceTier: &v} }, "service_tier")},
	command.OpStatus: {"GET", "/sessions/{id}", func(_ context.Context, s *Session, _ map[string]any) (any, error) {
		return s.View(), nil
	}},
	command.OpQueueList: {"", "", func(_ context.Context, s *Session, _ map[string]any) (any, error) {
		return append([]string{}, s.View().Queued...), nil
	}},
	command.OpProcessList: {"GET", "/processes", func(_ context.Context, s *Session, _ map[string]any) (any, error) {
		if s.r.procs == nil {
			return []process.Info{}, nil
		}
		return s.r.procs.List(), nil
	}},
}

// errNoFold reports a compaction that appended nothing.
var errNoFold = errors.New("/compact did nothing: the session does not have enough turns yet to fold")

// compact runs Session.Compact, which always keeps compaction_keep_turns.
func compact(ctx context.Context, s *Session, args map[string]any) (any, error) {
	if _, ok := args["keep_turns"]; ok {
		return nil, fmt.Errorf("%w: /compact takes no keep_turns; compaction_keep_turns sets it", ErrInvalidRequest)
	}
	ran, err := s.a.Compact(ctx)
	if err == nil && !ran {
		err = errNoFold
	}
	return nil, err
}

const (
	unsupportedReason = "Not available in this client."
	commandResultCap  = 16 << 10
)

func supported(spec *command.Spec) bool {
	_, ok := ops[spec.Op]
	return spec.Kind == command.KindControl && ok
}

// commandDirs returns the prompt-command dirs of a WorkDir: commands_dirs,
// relative to it, or its .agents/commands. No WorkDir has none.
func commandDirs(workDir string, dirs []string) []string {
	if workDir == "" {
		return nil
	}
	if dirs == nil {
		return []string{filepath.Join(workDir, ".agents", "commands")}
	}
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d
		if !filepath.IsAbs(d) {
			out[i] = filepath.Join(workDir, d)
		}
	}
	return out
}

// Commands returns the slash-command menu: every built-in command and the
// prompt commands of the WorkDir, which it reads on each call.
func (r *Runtime) Commands() (protocol.Commands, error) {
	prompts, invalid, err := command.DiscoverWithErrors(r.commandDirs)
	if err != nil {
		return protocol.Commands{}, err
	}
	out := protocol.Commands{Commands: []protocol.CommandEntry{}, ServeSupport: map[string]protocol.CommandSupport{}}
	for _, spec := range command.NewRegistry().All() {
		out.Commands = append(out.Commands, commandEntry(spec))
		out.ServeSupport[spec.Name] = protocol.CommandSupport{Supported: true}
		if !supported(spec) {
			out.ServeSupport[spec.Name] = protocol.CommandSupport{Reason: unsupportedReason}
		}
	}
	for _, p := range prompts {
		out.Commands = append(out.Commands, protocol.CommandEntry{Name: p.Name, Kind: string(command.KindPrompt),
			Summary: p.Description, ArgHint: p.ArgHint, Category: string(command.CategoryInfo)})
		out.ServeSupport[p.Name] = protocol.CommandSupport{Supported: true}
	}
	for _, failed := range invalid {
		if failed.Name == "" {
			out.DiscoveryErrors = append(out.DiscoveryErrors, failed.Reason)
			continue
		}
		out.Commands = append(out.Commands, protocol.CommandEntry{Name: failed.Name, Kind: string(command.KindPrompt), Category: string(command.CategoryInfo)})
		out.ServeSupport[failed.Name] = protocol.CommandSupport{Reason: failed.Reason}
	}
	slices.SortFunc(out.Commands, func(a, b protocol.CommandEntry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func commandEntry(spec *command.Spec) protocol.CommandEntry {
	e := protocol.CommandEntry{Name: spec.Name, Aliases: spec.Aliases, Kind: string(spec.Kind), Op: string(spec.Op),
		Summary: spec.Summary, ArgHint: spec.ArgHint, Category: string(spec.Category), Destructive: spec.Destructive}
	for _, a := range spec.Args {
		e.Args = append(e.Args, protocol.CommandArg{Name: a.Name, Type: string(a.Type), Optional: a.Optional})
	}
	if o, ok := ops[spec.Op]; ok {
		e.Method, e.Path = o.method, o.path
		e.AvailableDuringTask = &spec.AvailableDuringTask
	}
	// compaction_keep_turns sets keep_turns, so /compact takes no args.
	if spec.Op == command.OpCompact {
		e.ArgHint, e.Args = "", nil
	}
	return e
}

// plan is the resolution of a typed command: its first record, the record
// that replaces it while a run is on, and the resolution to dispatch.
type plan struct {
	rec  eventlog.CommandRecorded
	busy *eventlog.CommandRecorded
	res  *command.Resolution
}

// resolve resolves the typed line of in. It returns a plan for a command,
// or the input to admit: in, with a literal leading slash for "//", or
// with the text of a prompt command and source command.
func (s *Session) resolve(in protocol.Input) (*plan, protocol.Input, error) {
	line := in.Parts[0].Text
	res, err := command.NewRegistry().Resolve(line)
	unknown, isUnknown := errors.AsType[*command.UnknownCommandError](err)
	argsErr, isArgs := errors.AsType[*command.ArgsError](err)
	switch {
	case errors.Is(err, command.ErrNotCommand):
		in.Parts = []protocol.Part{{Type: protocol.PartText, Text: res.Text}}
		return nil, in, nil
	case isUnknown:
		return s.prompt(in, unknown.Name)
	case isArgs:
		return &plan{rec: record(in.ID, line, argsErr.Spec.Name, nil, protocol.CommandFailed, err.Error())}, in, nil
	case err != nil:
		return nil, in, err
	}
	typed := session.Typed(line)
	if !supported(res.Spec) {
		return &plan{rec: record(in.ID, line, res.Spec.Name, res.Args, protocol.CommandUnsupported, "/"+typed+" is not available in this client")}, in, nil
	}
	p := &plan{rec: record(in.ID, line, res.Spec.Name, res.Args, protocol.CommandAccepted, ""), res: &res}
	if !res.Spec.AvailableDuringTask {
		busy := record(in.ID, line, res.Spec.Name, res.Args, protocol.CommandRefused, refusal(typed))
		p.busy = &busy
	}
	return p, in, nil
}

func refusal(typed string) string {
	return "/" + typed + " cannot run while a turn is running; send it again after the turn ends"
}

func record(id, line, name string, args map[string]any, status, text string) eventlog.CommandRecorded {
	if len(args) == 0 {
		args = nil
	}
	return eventlog.CommandRecorded{InputID: id, Line: line, Name: name, Args: args, Status: status, Text: text}
}

// prompt returns in as the expanded text of the prompt command name, or as
// is when the WorkDir has no such command.
func (s *Session) prompt(in protocol.Input, name string) (*plan, protocol.Input, error) {
	p, err := command.LookupPrompt(s.r.commandDirs, name)
	if err != nil || p == nil {
		return nil, in, err
	}
	body, err := p.LoadBody()
	if err != nil {
		return nil, in, err
	}
	args := strings.TrimSpace(strings.TrimPrefix(in.Parts[0].Text, "/"+name))
	expanded := command.Expand(body, args)
	if strings.TrimSpace(expanded) == "" {
		return nil, in, fmt.Errorf("%w: prompt command expanded to empty text", ErrInvalidRequest)
	}
	in.Parts, in.Source = []protocol.Part{{Type: protocol.PartText, Text: expanded}}, "command"
	return nil, in, nil
}

// command records the first status of p. An accepted command joins the
// work that Runtime.Close waits for before its record, so Close refuses it.
func (s *Session) command(ctx context.Context, p *plan) (protocol.Admitted, bool, error) {
	if p.res != nil {
		if err := s.r.hold(); err != nil {
			return protocol.Admitted{}, false, err
		}
	}
	rec, seq, repeat, err := s.a.Record(ctx, p.rec, p.busy)
	switch {
	case p.res == nil:
	case err == nil && !repeat && rec.Status == protocol.CommandAccepted:
		go func() {
			defer s.r.group.Done()
			s.dispatch(rec, *p.res)
		}()
	default:
		s.r.group.Done()
	}
	if err != nil {
		return protocol.Admitted{}, false, err
	}
	return protocol.Admitted{InputID: rec.InputID, Seq: seq, Command: rec.Status}, repeat, nil
}

// dispatch runs the operation of an accepted command and records its outcome.
func (s *Session) dispatch(rec eventlog.CommandRecorded, res command.Resolution) {
	result, err := ops[res.Spec.Op].run(s.r.base, s, res.Args)
	rec.Status, rec.Text, rec.Result, rec.ResultTruncated = outcome(session.Typed(rec.Line), res.Spec, result, err)
	_, _, _, _ = s.a.Record(s.r.base, rec, nil)
}

// codedErrors are the errors whose text a command record shows; any other
// error can hold store details.
var codedErrors = []error{ErrInvalidRequest, ErrSessionNotFound, ErrInputConflict, ErrTurnMismatch, ErrSessionBusy, ErrModelUnavailable}

func outcome(typed string, spec *command.Spec, result any, err error) (status, text string, raw json.RawMessage, truncated bool) {
	switch {
	case err == nil:
		text = "/" + typed + " succeeded"
		if result == nil {
			return protocol.CommandSucceeded, text, nil, false
		}
		raw, err := json.Marshal(result)
		if err != nil || len(raw) > commandResultCap {
			return protocol.CommandSucceeded, text, nil, err == nil
		}
		return protocol.CommandSucceeded, text, raw, false
	case errors.Is(err, errNoFold):
		return protocol.CommandFailed, err.Error(), nil, false
	case errors.Is(err, ErrSessionBusy) && !spec.AvailableDuringTask:
		return protocol.CommandRefused, refusal(typed), nil, false
	case errors.Is(err, ErrDraining), errors.Is(err, ErrSessionNotOwned), errors.Is(err, context.Canceled):
		return protocol.CommandInterrupted, "harness stopped before /" + typed + " finished; it will not run again", nil, false
	case slices.ContainsFunc(codedErrors, func(c error) bool { return errors.Is(err, c) }):
		return protocol.CommandFailed, err.Error(), nil, false
	}
	return protocol.CommandFailed, "/" + typed + " failed: internal error", nil, false
}
