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
	"sync"
	"sync/atomic"
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
	maxTokens      int
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
	fs.IntVar(&opts.maxTokens, "max-tokens", 0, "per-response output token cap")
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
	slog.SetDefault(logger)
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
	rt, err := harness.New(harness.Options{Store: store, Config: *cfg, WorkDir: workDir, Version: version, MaxTokens: opts.maxTokens})
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
	out := &printer{out: os.Stdout, errW: os.Stderr, jsonOut: opts.jsonOut, enc: json.NewEncoder(os.Stdout), rootID: s.View().ID,
		streamed: map[string]bool{}, names: map[string]string{}, open: rt.Open, store: store, followers: map[string]*follower{}, closing: make(chan struct{})}
	out.markExisting(ctx, out.rootID, map[string]bool{})
	runErr := drive(ctx, s, opts, out)
	out.finishChildren()
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
// model, and a retry. With jsonOut it prints each event as one JSON line with
// the ID of its session. It follows each task child of the run, which the
// runtime runs, as the engine printed them through its shared callback.
type printer struct {
	out, errW   io.Writer
	jsonOut     bool
	enc         *json.Encoder
	rootID      string
	streamed    map[string]bool
	names       map[string]string
	printedText bool
	// open opens a task child to follow it.
	open  func(context.Context, string) (*harness.Session, error)
	store harness.Store
	mu    sync.Mutex
	// followers holds each child that the run follows, by session ID.
	followers map[string]*follower
	// unsettled holds the followed children that the log of the root session
	// has not recorded as settled. A child settles in the log of its parent
	// together with its report, so the run does not finish before it.
	unsettled map[string]bool
	children  sync.WaitGroup
	// closing closes when the run has settled: a follower then prints up to
	// the head that its child holds and returns.
	closing chan struct{}
	// streamedThis is set when text streamed since the last completed item.
	streamedThis bool
	command      *protocol.Event
}

// follower is the printing of one task child. mark is the seq of the newest
// record that the run has printed or need not print: a child that an earlier
// run spawned starts at its head when the run starts, so that only the records
// of the run print. again is set when a spawn of the child arrives while a
// pass runs.
type follower struct {
	mark           uint64
	running, again bool
}

// jsonLine is an event with the ID of its session.
type jsonLine struct {
	SessionID string `json:"session_id"`
	protocol.Event
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
		p.handle(p.rootID, ev)
		p.follow(ctx, ev)
		p.track(ev)
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
		p.handle(p.rootID, ev)
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
	if p.waiting() {
		return false
	}
	v := viewAt(s, ev.Seq)
	return settled(v) && ev.Seq >= v.HeadSeq
}

// track records the spawn and the settlement of a task child of the root
// session.
func (p *printer) track(ev protocol.Event) {
	if p.open == nil || ev.Kind != "child.spawned" && ev.Kind != "child.settled" {
		return
	}
	var c struct {
		ChildID string `json:"child_id"`
	}
	if json.Unmarshal(ev.Data, &c) != nil || c.ChildID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ev.Kind == "child.settled" {
		delete(p.unsettled, c.ChildID)
		return
	}
	if p.unsettled == nil {
		p.unsettled = map[string]bool{}
	}
	p.unsettled[c.ChildID] = true
}

// waiting reports whether the root log has not yet recorded child.settled
// for a task child of the root session.
func (p *printer) waiting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.unsettled) > 0
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

func (p *printer) handle(id string, ev protocol.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ev.Kind == "command.recorded" && id == p.rootID {
		p.command = &ev
	}
	if p.jsonOut {
		_ = p.enc.Encode(jsonLine{SessionID: id, Event: ev})
		return
	}
	switch ev.Kind {
	case protocol.KindItemDelta:
		var f protocol.ItemFrame
		if json.Unmarshal(ev.Data, &f) == nil && f.Type == "text" {
			_, _ = fmt.Fprint(p.out, f.Text)
			p.printedText, p.streamedThis, p.streamed[f.ItemID] = true, true, true
		}
	case protocol.KindStatus:
		var f protocol.StatusFrame
		if json.Unmarshal(ev.Data, &f) == nil && f.Status == protocol.StatusRetrying && p.streamedThis {
			_, _ = fmt.Fprintln(p.out)
			_, _ = fmt.Fprintln(p.errW, "[re-streaming after a transient provider error]")
			p.streamedThis = false
		}
	case "item.completed":
		p.item(ev)
	}
}

// item prints the tools of a completed item, and the text of an item that
// streamed no delta.
func (p *printer) item(ev protocol.Event) {
	p.streamedThis = false
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
		case v.LastTurn != nil && v.LastTurn.Error != "":
			return errors.New(v.LastTurn.Error)
		default:
			fmt.Fprintf(os.Stderr, "goal not achieved after %d turn(s): %s\n", g.Turns, g.Reason)
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

// follow starts the printing of the task child that ev names, if any.
func (p *printer) follow(ctx context.Context, ev protocol.Event) {
	if p.open == nil || ev.Kind != "child.spawned" {
		return
	}
	var c struct {
		ChildID string `json:"child_id"`
	}
	if json.Unmarshal(ev.Data, &c) != nil || c.ChildID == "" {
		return
	}
	f := p.followerOf(c.ChildID)
	p.mu.Lock()
	if f.running {
		f.again = true
		p.mu.Unlock()
		return
	}
	f.running = true
	p.mu.Unlock()
	p.children.Go(func() {
		for {
			p.printChild(ctx, c.ChildID, f)
			p.mu.Lock()
			if !f.again {
				f.running = false
				p.mu.Unlock()
				return
			}
			f.again = false
			p.mu.Unlock()
		}
	})
}

func (p *printer) followerOf(id string) *follower {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.followers[id]
	if f == nil {
		f = &follower{}
		p.followers[id] = f
	}
	return f
}

// markExisting sets the mark of each task child that session id spawned in an
// earlier run, and of their children, to the head of the child. A child that
// the run spawns again then prints only what the new input adds.
func (p *printer) markExisting(ctx context.Context, id string, seen map[string]bool) {
	v, err := harness.OpenView(ctx, p.store, id)
	if err != nil {
		return
	}
	for ev, err := range v.Events(ctx, 0) {
		if err != nil {
			return
		}
		if ev.Kind != "child.spawned" {
			continue
		}
		var c struct {
			ChildID string `json:"child_id"`
		}
		if json.Unmarshal(ev.Data, &c) != nil || c.ChildID == "" || seen[c.ChildID] {
			continue
		}
		seen[c.ChildID] = true
		cv, err := harness.OpenView(ctx, p.store, c.ChildID)
		if err != nil {
			continue
		}
		f := p.followerOf(c.ChildID)
		p.mu.Lock()
		f.mark = cv.Session().HeadSeq
		p.mu.Unlock()
		p.markExisting(ctx, c.ChildID, seen)
	}
}

// openChild opens a child that its parent has just spawned. The runtime
// creates the child after the parent records the spawn, so it waits for the
// log of the child to hold a record before it opens the child; it ends when
// the run settles.
func (p *printer) openChild(ctx context.Context, id string) *harness.Session {
	for {
		if head, err := p.store.Head(ctx, id); err == nil && head > 0 {
			if cs, err := p.open(ctx, id); err == nil {
				return cs
			}
		}
		select {
		case <-p.closing:
			return nil
		case <-ctx.Done():
			return nil
		case <-time.After(openChildWait):
		}
	}
}

// openChildWait is the wait between two opens of a child that does not exist yet.
const openChildWait = 5 * time.Millisecond

// printChild prints the records of a child after the mark of f until the
// child has settled. A child that waits for the input of a new spawn has not
// settled before that input lands: the pass does not end before it has seen
// an input.admitted record.
func (p *printer) printChild(ctx context.Context, id string, f *follower) {
	cs := p.openChild(ctx, id)
	if cs == nil {
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.mu.Lock()
	start := f.mark
	p.mu.Unlock()
	var last, target atomic.Uint64
	last.Store(start)
	go func() {
		select {
		case <-p.closing:
			target.Store(cs.View().HeadSeq)
			if last.Load() >= target.Load() {
				cancel()
			}
		case <-sctx.Done():
		}
	}()
	landed := false
	for ev, err := range cs.Events(sctx, start) {
		if err != nil {
			return
		}
		p.handle(id, ev)
		p.follow(ctx, ev)
		if ev.Ephemeral {
			continue
		}
		last.Store(ev.Seq)
		p.mu.Lock()
		f.mark = ev.Seq
		p.mu.Unlock()
		landed = landed || ev.Kind == "input.admitted"
		if t := target.Load(); t != 0 && ev.Seq >= t {
			return
		}
		if !landed {
			continue
		}
		if v := viewAt(cs, ev.Seq); settled(v) && ev.Seq >= v.HeadSeq {
			return
		}
	}
}

// finishChildren ends the followers: each prints up to the head that its
// child holds now, then returns.
func (p *printer) finishChildren() {
	close(p.closing)
	p.children.Wait()
}
