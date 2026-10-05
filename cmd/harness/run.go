package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// errGoalNotAchieved makes `harness run -goal` exit 3. Its final status is on
// stderr already.
var errGoalNotAchieved = errors.New("goal not achieved")

type runOptions struct {
	prompt         string
	goal           string
	goalMaxTurns   int
	model          string
	system         string
	jsonOut        bool
	noSave         bool
	noInstructions bool
	skillsDirs     []string
	agentDefsDirs  []string
	resume         string
	cont           bool
}

func runFlags(opts *runOptions) *flag.FlagSet {
	if opts == nil {
		opts = &runOptions{}
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.prompt, "p", "", "the prompt (required unless -goal is given)")
	fs.StringVar(&opts.goal, "goal", "", "pursue a goal: run this condition as a turn, then run turns with evaluator feedback until an independent evaluator judges it met (requires config goal_evaluator_model)")
	fs.IntVar(&opts.goalMaxTurns, "goal-max-turns", 0, "maximum turns for -goal (0 = unlimited)")
	fs.StringVar(&opts.model, "model", "", "model ref (provider/model) or alias; overrides the persisted model when resuming; default from config, else "+config.DefaultModel)
	fs.StringVar(&opts.system, "system", "", "extra system prompt segment; appended after any config append_system_prompt segments")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit the event stream as JSON lines instead of text")
	fs.BoolVar(&opts.noSave, "no-save", false, "disable session persistence")
	fs.BoolVar(&opts.noInstructions, "no-instructions", false, "do not inject the project's AGENTS.md into the system prompt")
	fs.Func("skills-dir", "directory of Agent Skills to advertise (repeatable); overrides config skills_dirs; default <workdir>/.agents/skills when present", func(v string) error {
		opts.skillsDirs = append(opts.skillsDirs, v)
		return nil
	})
	fs.Func("agent-def-dir", "directory of custom task-tool agent definitions to advertise (repeatable); overrides config agent_defs_dirs; default <workdir>/.agents", func(v string) error {
		opts.agentDefsDirs = append(opts.agentDefsDirs, v)
		return nil
	})
	fs.StringVar(&opts.resume, "r", "", "resume the session with this id")
	fs.StringVar(&opts.resume, "resume", "", "resume the session with this id")
	fs.BoolVar(&opts.cont, "c", false, "continue the most recent session")
	fs.BoolVar(&opts.cont, "continue", false, "continue the most recent session")
	return fs
}

// runCmd runs one prompt or one goal on a Runtime of the working directory,
// and prints the stream of the session.
func runCmd(args []string) error {
	var opts runOptions
	fs := runFlags(&opts)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var modelSet bool
	fs.Visit(func(f *flag.Flag) { modelSet = modelSet || f.Name == "model" })
	switch {
	case opts.prompt == "" && opts.goal == "":
		return fmt.Errorf("-p <prompt> or -goal <condition> is required")
	case opts.prompt != "" && opts.goal != "":
		return fmt.Errorf("-p and -goal are mutually exclusive")
	case opts.resume != "" && opts.cont:
		return fmt.Errorf("-r and -c are mutually exclusive")
	case (opts.resume != "" || opts.cont) && opts.noSave:
		return fmt.Errorf("cannot resume a session with -no-save")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := loadConfigLogged(logger)
	if err != nil {
		return err
	}
	if err := applyOverrides(cfg, opts.noInstructions, opts.skillsDirs, opts.agentDefsDirs); err != nil {
		return err
	}
	if opts.system != "" {
		cfg.AppendSystemPrompt = append(cfg.AppendSystemPrompt[:len(cfg.AppendSystemPrompt):len(cfg.AppendSystemPrompt)], opts.system)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	store, err := runStore(cfg, opts.noSave)
	if err != nil {
		return err
	}
	rt, err := harness.New(harness.Options{Store: store, Config: *cfg, WorkDir: workDir, Version: version})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	line, err := checkLine(cfg, rt, opts, modelSet)
	if err != nil {
		return errors.Join(err, closeRuntime(rt))
	}
	s, err := runSession(ctx, rt, store, cfg, opts, modelSet, line)
	if err != nil {
		return errors.Join(err, closeRuntime(rt))
	}
	out := &printer{out: os.Stdout, errW: os.Stderr, jsonOut: opts.jsonOut, enc: json.NewEncoder(os.Stdout),
		streamed: map[string]bool{}, names: map[string]string{}}
	runErr := drive(ctx, s, opts, out)
	if out.printedText {
		fmt.Println()
	}
	if !opts.noSave && (runErr == nil || errors.Is(runErr, errGoalNotAchieved)) {
		fmt.Fprintln(os.Stderr, "session:", s.View().ID)
	}
	return errors.Join(runErr, closeRuntime(rt))
}

// runStore is a DiskStore in the session dir, or a MemStore with -no-save.
func runStore(cfg *config.Config, noSave bool) (harness.Store, error) {
	if noSave {
		return harness.NewMemStore(), nil
	}
	dir, err := sessionDir(cfg.SessionDir)
	if err != nil {
		return nil, err
	}
	return harness.NewDiskStore(dir), nil
}

// runSession creates the session of the run, or opens the one that -r or -c
// names. An explicit -model replaces the model of an opened session. A line
// that the model of that session cannot take is refused before the open.
func runSession(ctx context.Context, rt *harness.Runtime, store harness.Store, cfg *config.Config, opts runOptions, modelSet bool, line *unknownLine) (*harness.Session, error) {
	model := ""
	if modelSet {
		model = cfg.ResolveModel(opts.model)
	}
	id := opts.resume
	if opts.cont {
		last, err := lastSession(ctx, rt)
		if err != nil {
			return nil, err
		}
		id = last
	}
	if id == "" {
		return rt.Create(ctx, protocol.CreateSession{Model: model})
	}
	if line != nil {
		v, err := harness.OpenView(ctx, store, id)
		if err != nil {
			return nil, err
		}
		if err := line.refuseOn(v.Session().Model); err != nil {
			return nil, err
		}
	}
	s, err := rt.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	if model != "" {
		if _, err := s.Update(ctx, protocol.SettingsPatch{Model: &model}); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// lastSession returns the ID of the newest session of the store.
func lastSession(ctx context.Context, rt *harness.Runtime) (string, error) {
	last := ""
	for after := ""; ; {
		page, err := rt.List(ctx, protocol.ListSessions{After: after})
		if err != nil {
			return "", err
		}
		if n := len(page.Sessions); n > 0 {
			last = page.Sessions[n-1].ID
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if last == "" {
		return "", fmt.Errorf("no sessions to continue")
	}
	return last, nil
}

// drive starts the work of the run and prints the stream of the session until
// it settles.
func drive(ctx context.Context, s *harness.Session, opts runOptions, out *printer) error {
	head := s.View().HeadSeq
	var admitted protocol.Admitted
	if opts.goal != "" {
		if err := s.SetGoal(ctx, protocol.Goal{Condition: opts.goal, MaxTurns: opts.goalMaxTurns}); err != nil {
			return err
		}
	} else {
		id, err := newInputID()
		if err != nil {
			return err
		}
		admitted, err = s.Submit(ctx, protocol.Input{ID: id, Source: protocol.SourceTyped,
			Parts: []protocol.Part{{Type: protocol.PartText, Text: opts.prompt}}})
		if err != nil {
			return err
		}
	}
	if err := out.stream(ctx, s, head, admitted.Command != ""); err != nil {
		return err
	}
	return out.verdict(s, opts, admitted)
}

func newInputID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(b), nil
}

// settled reports whether a session runs nothing and has nothing to run: no
// turn, no queued input that can start (a turn that ended provider_exhausted
// holds its queue), and no active goal.
func settled(v protocol.Session) bool {
	held := v.LastTurn != nil && v.LastTurn.Cause == "provider_exhausted"
	idle := v.Status == protocol.StatusIdle || v.Status == protocol.StatusWaiting
	return idle && (len(v.Queued) == 0 || held) && (v.Goal == nil || v.Goal.State != "active")
}

// printer renders the events of a run: text as it streams, the tools of the
// model, and a retry. With jsonOut it prints each event as one JSON line.
type printer struct {
	out, errW   io.Writer
	jsonOut     bool
	enc         *json.Encoder
	streamed    map[string]bool
	names       map[string]string
	printedText bool
	command     *protocol.Event
}

// stream prints each event after seq, and returns when the session has
// settled. A typed command is settled when its newest record is final.
func (p *printer) stream(ctx context.Context, s *harness.Session, after uint64, command bool) error {
	if !command && settled(s.View()) && s.View().HeadSeq > after {
		return p.drain(ctx, s, after)
	}
	for ev, err := range s.Events(ctx, after) {
		if err != nil {
			return err
		}
		p.handle(ev)
		if !ev.Ephemeral && p.finished(s, ev, command) {
			return nil
		}
	}
	return ctx.Err()
}

// drain prints the records that a settled session holds after seq.
func (p *printer) drain(ctx context.Context, s *harness.Session, after uint64) error {
	head := s.View().HeadSeq
	for ev, err := range s.Events(ctx, after) {
		if err != nil {
			return err
		}
		p.handle(ev)
		if !ev.Ephemeral && ev.Seq >= head {
			break
		}
	}
	return nil
}

func (p *printer) finished(s *harness.Session, ev protocol.Event, command bool) bool {
	if command {
		return ev.Kind == "command.recorded" && commandFinal(ev) && settled(s.View())
	}
	v := viewAt(s, ev.Seq)
	return settled(v) && ev.Seq >= v.HeadSeq
}

// viewAt returns the view of s once it holds seq. The actor publishes a view
// after it appends a record, so the view of a record that a reader has just
// received can still be the one before it.
func viewAt(s *harness.Session, seq uint64) protocol.Session {
	v := s.View()
	for range viewWaits {
		if v.HeadSeq >= seq {
			break
		}
		time.Sleep(time.Millisecond)
		v = s.View()
	}
	return v
}

// viewWaits bounds the wait of viewAt to about a second.
const viewWaits = 1000

func commandFinal(ev protocol.Event) bool {
	var c struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(ev.Data, &c) == nil && c.Status != protocol.CommandAccepted
}

type loggedPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	IsError   bool            `json:"is_error"`
}

func (p *printer) handle(ev protocol.Event) {
	if ev.Kind == "command.recorded" {
		p.command = &ev
	}
	if p.jsonOut {
		_ = p.enc.Encode(ev)
		return
	}
	switch ev.Kind {
	case protocol.KindItemDelta:
		var f protocol.ItemFrame
		if json.Unmarshal(ev.Data, &f) == nil && f.Type == "text" {
			_, _ = fmt.Fprint(p.out, f.Text)
			p.printedText, p.streamed[f.ItemID] = true, true
		}
	case protocol.KindStatus:
		var f protocol.StatusFrame
		if json.Unmarshal(ev.Data, &f) == nil && f.Status == protocol.StatusRetrying {
			if p.printedText {
				_, _ = fmt.Fprintln(p.out)
			}
			_, _ = fmt.Fprintln(p.errW, "[re-streaming after a transient provider error]")
		}
	case "item.completed":
		p.item(ev)
	}
}

// item prints the tools of a completed item, and the text of an item that
// streamed no delta.
func (p *printer) item(ev protocol.Event) {
	var it struct {
		ItemID  string `json:"item_id"`
		Message struct {
			Parts []loggedPart `json:"parts"`
		} `json:"message"`
	}
	if json.Unmarshal(ev.Data, &it) != nil {
		return
	}
	for _, part := range it.Message.Parts {
		switch part.Type {
		case "text":
			if !p.streamed[it.ItemID] {
				_, _ = fmt.Fprint(p.out, part.Text)
				p.printedText = true
			}
		case "tool_call":
			p.names[part.CallID] = part.Name
			_, _ = fmt.Fprintf(p.errW, "\n[tool %s] %s\n", part.Name, part.Arguments)
		case "tool_result":
			if part.IsError {
				_, _ = fmt.Fprintf(p.errW, "[tool %s failed] %s\n", p.names[part.CallID], part.Text)
			}
		}
	}
}

// verdict returns the failure of the run: a turn that failed, a typed command
// that did not succeed, or a goal that was not achieved.
func (p *printer) verdict(s *harness.Session, opts runOptions, admitted protocol.Admitted) error {
	v := s.View()
	if opts.goal != "" {
		g := v.Goal
		switch {
		case g == nil:
			return fmt.Errorf("the goal was cleared")
		case g.State == "achieved":
			fmt.Fprintf(os.Stderr, "goal achieved in %d turn(s): %s\n", g.Turns, g.Reason)
			return nil
		default:
			fmt.Fprintf(os.Stderr, "goal not achieved after %d turn(s): %s (%s)\n", g.Turns, g.Reason, g.State)
			return errGoalNotAchieved
		}
	}
	if admitted.Command != "" {
		return p.commandResult()
	}
	if t := v.LastTurn; t != nil && t.Error != "" {
		return errors.New(t.Error)
	}
	return nil
}

// commandResult returns the failure of a typed command. A command that
// succeeded prints nothing.
func (p *printer) commandResult() error {
	if p.command == nil {
		return nil
	}
	var c struct {
		Status string `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(p.command.Data, &c); err != nil {
		return err
	}
	if c.Status == protocol.CommandSucceeded {
		return nil
	}
	return errors.New(c.Text)
}
