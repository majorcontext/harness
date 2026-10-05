package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/majorcontext/harness/internal/backend/external"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// run is one CLI run: it maps the frames of the CLI to items of the turn.
type run struct {
	out       turn.Sink
	turnID    string
	proc      *external.Process
	tools     *external.Tools
	mcpConfig string
	dir       string
	mirror    external.Mirror
	saved     []byte
	allowed   map[string]bool
	bridged   map[string]bool
	names     map[string]string
	// questions reports a run with the question channel; resolution answers
	// the parked call, and question is the call that the main thread asked.
	questions  bool
	denied     bool
	resolution *resolution
	question   *question
	// read returns the bytes of an attachment of the turn.
	read func(key string) ([]byte, error)
	// continues reports a run of a turn whose input the CLI already took;
	// taken reports that this run gave the CLI the input.
	continues bool
	taken     bool
	stopped   bool

	// limits is the newest subscription snapshot of the run; the result
	// reports it with the usage of its call.
	limits  *eventlog.SubscriptionUsage
	started bool
	sendErr error
	tailErr error
	result  *envelope
	// pending is the assistant item that later frames of the same API
	// response join; deferred holds the tool results that follow it.
	pending   *eventlog.Message
	pendingID string
	deferred  []eventlog.Message
	// steer reports a steer input that waits for Sink.Steer. A resolution
	// run takes none and leaves the notification for the run after it: the
	// CLI would queue the input behind its own continuation and answer it in a
	// second result.
	steer      bool
	mainModel  string
	lastCall   *usage
	compact    string
	sawCompact bool
}

// dismissing reports a run that only denies the parked call.
func (r *run) dismissing() bool { return r.resolution != nil && r.resolution.dismiss }

func (r *run) cleanup() {
	if r.tools != nil {
		r.tools.Close()
	}
	for _, p := range []string{r.dir, r.mcpConfig} {
		if p != "" {
			_ = os.RemoveAll(p)
		}
	}
}

// drive sends the prompt and handles frames until the result, the end of
// stdout, or the end of ctx.
func (r *run) drive(ctx context.Context, req turn.Request) error {
	r.read = func(key string) ([]byte, error) { return req.Blob(ctx, key) }
	if r.resolution == nil {
		line, err := r.prompt(req)
		if err != nil {
			return err
		}
		r.sendErr = r.proc.Send(line)
	}
	steered := req.Steered
	if r.resolution != nil {
		steered = nil
	}
	for {
		select {
		case line, ok := <-r.proc.Lines():
			if !ok {
				return errExited
			}
			if err := r.frame(line); err != nil || r.result != nil {
				return err
			}
		case <-steered:
			r.steer = true
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		if r.steer {
			if err := r.steered(); err != nil {
				return err
			}
		}
	}
}

var errExited = errors.New("claudecode: the CLI exited before its result")

// finish ends the process, maps the frames that it writes until it exits,
// saves the external session, and returns the turn error. A stopped turn
// keeps every item that the CLI wrote, and completes on a success result.
func (r *run) finish(ctx context.Context, err error) error {
	r.stopped = ctx.Err() != nil
	if err == nil {
		r.proc.CloseInput()
	} else if !r.stopped {
		r.proc.Interrupt()
	}
	exit := r.proc.Finish(grace, r.tail)
	if r.stopped {
		err = stopError(err, context.Cause(ctx), r.result)
	}
	err = r.outcome(errors.Join(err, r.tailErr, r.flush()), exit)
	if r.dismissing() && (r.result != nil || r.denied) && !r.stopped {
		err = nil
	}
	if r.resolution != nil && err == nil && r.mirror.Parked == r.resolution.callID {
		r.mirror.Parked = ""
	}
	if r.taken && err != nil && (errors.Is(context.Cause(ctx), turn.ErrHandoff) || errors.Is(err, turn.ErrRetryable)) {
		r.mirror.Turn = r.turnID
	}
	if r.mirror.SessionID != "" {
		if serr := r.save(); err == nil {
			err = serr
		}
	}
	return err
}

// stopError returns the turn error of a run that the stop cause ended.
// drive returned err; a stopped run completes only on a success result.
func stopError(err, cause error, res *envelope) error {
	if err != nil && !errors.Is(err, errExited) && !errors.Is(err, cause) {
		return err
	}
	if res == nil || res.IsError {
		return cause
	}
	return nil
}

// outcome returns the turn error of a run that ended with err and exit.
func (r *run) outcome(err, exit error) error {
	switch {
	case errors.Is(err, errExited):
		if cause := errors.Join(exit, r.sendErr); cause != nil {
			err = fmt.Errorf("%w: %w", err, cause)
		}
		if r.started {
			err = fmt.Errorf("%w: %w", turn.ErrRetryable, err)
		}
		return err
	case err != nil:
		return err
	}
	if res := r.result; res.IsError {
		err = fmt.Errorf("claudecode: the turn failed (%s): %s", res.Subtype, res.Result)
		if retryable(res.Subtype, res.Result) {
			err = fmt.Errorf("%w: %w", turn.ErrRetryable, err)
		}
	}
	return err
}

// save saves the external session when it changed since the last save.
func (r *run) save() error {
	blob, err := r.mirror.Encode()
	if err != nil || bytes.Equal(blob, r.saved) {
		return err
	}
	if err := r.out.SaveState(stateKey, blob); err != nil {
		return err
	}
	r.saved = blob
	return nil
}

// tail maps a frame that the CLI writes after its result or after the turn
// failed: only its transcript and its result. After a stop, it maps every
// frame: the CLI still writes the items that it already ran.
func (r *run) tail(line []byte) {
	var env envelope
	if json.Unmarshal(line, &env) != nil || env.Type == "result" && r.result != nil {
		return
	}
	var err error
	switch {
	case env.Type == "result":
		if r.placeholder(env) {
			return
		}
		r.result = &env
		err = r.settle(env)
	case r.stopped || env.Type == "transcript_mirror":
		err = r.handle(env)
	}
	if r.tailErr == nil {
		r.tailErr = err
	}
}

func (r *run) frame(line []byte) error {
	var env envelope
	if json.Unmarshal(line, &env) != nil {
		return nil
	}
	return r.handle(env)
}

func (r *run) handle(env envelope) error {
	if r.allowed != nil && !r.started && (env.Type == "assistant" || env.Type == "user" || env.Type == "result") {
		return fmt.Errorf("%w: %s frame before init", ErrToolsNotRestricted, env.Type)
	}
	if r.compact != "" && env.Type != "transcript_mirror" {
		if summary, err := r.compacted(env); err != nil || summary {
			return err
		}
	}
	if r.dismissing() && (env.Type == "assistant" || env.Type == "user") {
		return nil
	}
	switch env.Type {
	case "system":
		return r.system(env)
	case "assistant":
		return r.assistant(env)
	case "user":
		return r.toolResults(env)
	case "control_request":
		return r.control(env)
	case "result":
		if r.placeholder(env) {
			return nil
		}
		r.result = &env
		if r.dismissing() {
			return nil
		}
		return r.settle(env)
	case "transcript_mirror":
		return r.addMirror(env)
	case "rate_limit_event":
		if u := env.RateLimitInfo.subscription(); u != nil {
			r.limits = u
		}
	}
	return nil
}

func (r *run) addMirror(env envelope) error {
	if r.dir == "" {
		return nil
	}
	r.taken = true
	return r.mirror.Add(r.dir, env.FilePath, env.Entries)
}

func (r *run) system(env envelope) error {
	switch env.Subtype {
	case "init":
		r.started = true
		r.mainModel = env.Model
		if err := r.checkTools(env.Tools); err != nil {
			return err
		}
		r.taken = r.taken || r.dir == ""
		if env.SessionID != "" && env.SessionID != r.mirror.SessionID {
			r.mirror.SessionID = env.SessionID
			return r.save()
		}
	case "compact_boundary":
		r.sawCompact = true
		r.compact = "Claude Code compacted its context."
		if m := env.CompactMetadata; m != nil {
			r.compact = fmt.Sprintf("Claude Code compacted its context (%s, %d tokens before).", m.Trigger, m.PreTokens)
		}
	}
	return nil
}

func (r *run) checkTools(tools *[]string) error {
	if r.allowed == nil {
		return nil
	}
	if tools == nil {
		return fmt.Errorf("%w: the init frame lists no tools", ErrToolsNotRestricted)
	}
	for _, t := range *tools {
		if !r.allowed[t] {
			return fmt.Errorf("%w: %s", ErrToolsNotRestricted, t)
		}
	}
	return nil
}

// compacted records the pending compaction. A user frame with plain text
// right after the boundary is the summary; isSummary reports that env was it.
func (r *run) compacted(env envelope) (isSummary bool, err error) {
	summary := r.compact
	r.compact = ""
	var s string
	if env.Type == "user" && json.Unmarshal(decodeMessage(env.Message).Content, &s) == nil && s != "" {
		summary, isSummary = s, true
	}
	return isSummary, r.out.Compacted(summary)
}

func (r *run) assistant(env envelope) error {
	m := decodeMessage(env.Message)
	if m.Usage != nil && env.ParentToolUseID == "" {
		r.lastCall = m.Usage
	}
	parts := assistantParts(m)
	if len(parts) == 0 {
		return nil
	}
	if r.pending == nil || m.ID == "" || m.ID != r.pendingID || r.pending.ParentCallID != env.ParentToolUseID {
		if err := r.flush(); err != nil {
			return err
		}
		r.pending, r.pendingID = &eventlog.Message{Role: eventlog.RoleAssistant, ParentCallID: env.ParentToolUseID}, m.ID
	}
	for i, p := range parts {
		switch p.Type {
		case eventlog.PartText, eventlog.PartReasoning:
			r.out.Delta(r.pendingID, turn.Delta{Type: p.Type, Text: p.Text})
		case eventlog.PartToolCall:
			if name, ok := strings.CutPrefix(p.Name, mcpPrefix); ok && r.bridged[name] {
				parts[i].Name = name
			}
			r.names[p.CallID] = parts[i].Name
			if p.Name == askTool && env.ParentToolUseID == "" {
				r.question = &question{callID: p.CallID, input: p.Arguments}
			}
		}
	}
	r.pending.Parts = append(r.pending.Parts, parts...)
	if m.ID == "" {
		return r.flush()
	}
	return nil
}

func (r *run) toolResults(env envelope) error {
	parts := toolResults(decodeMessage(env.Message), r.names)
	if len(parts) == 0 {
		return nil
	}
	msg := eventlog.Message{Role: eventlog.RoleTool, Parts: parts, ParentCallID: env.ParentToolUseID}
	if r.pending != nil && r.pending.ParentCallID != env.ParentToolUseID {
		if err := r.flush(); err != nil {
			return err
		}
	}
	if r.pending != nil {
		r.deferred = append(r.deferred, msg)
		return nil
	}
	return r.out.Item(msg)
}

// flush records the pending assistant item, then its tool results.
func (r *run) flush() error {
	if r.pending == nil {
		return nil
	}
	items := append([]eventlog.Message{*r.pending}, r.deferred...)
	r.pending, r.pendingID, r.deferred = nil, "", nil
	for _, m := range items {
		if err := r.out.Item(m); err != nil {
			return err
		}
	}
	return nil
}

// steered writes the queued steer inputs to stdin.
func (r *run) steered() error {
	r.steer = false
	if err := r.flush(); err != nil {
		return err
	}
	msgs, err := r.out.Steer()
	for _, m := range msgs {
		if err != nil {
			break
		}
		var line input
		if line, err = userLine(m, r.read); err == nil {
			err = r.proc.Send(line)
		}
	}
	return err
}

// placeholder reports whether env is the empty zero-turn result that the
// CLI sends for a queued task notification. The real turn follows it. A
// compaction ends with the same shape and is final.
func (r *run) placeholder(env envelope) bool {
	return !env.IsError && env.NumTurns != nil && *env.NumTurns == 0 && env.Result == "" &&
		r.pending == nil && env.LocalCommand == "" && !r.sawCompact
}

// settle records the items and the telemetry of the result.
func (r *run) settle(env envelope) error {
	if err := r.flush(); err != nil {
		return err
	}
	r.telemetry(env)
	return r.ask()
}

func (r *run) telemetry(env envelope) {
	t := turn.Telemetry{Usage: env.Usage.usage(), SubscriptionUsage: r.limits}
	if w, tokens := env.ModelUsage[r.mainModel].ContextWindow, r.lastCall.prompt(); w > 0 || tokens > 0 {
		t.Context = eventlog.ContextMeasured{Tokens: tokens, Window: w, Source: stateKey}
	}
	r.out.Telemetry(t)
}
