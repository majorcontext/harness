package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/internal/backend/external"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// run is one CLI run: it maps the frames of the CLI to items of the turn.
type run struct {
	out     turn.Sink
	turnID  string
	proc    *external.Process
	tools   *external.Tools
	temps   []string
	dir     string
	mirror  external.Mirror
	allowed map[string]bool
	// restricted is a run whose session lists its allowed tools: the CLI gets
	// no configured MCP server.
	restricted bool
	bridged    map[string]bool
	// blobs holds the blobs of the bridge results by call ID until the result frame arrives.
	blobMu sync.Mutex
	blobs  map[string][]protocol.Blob
	names  map[string]string
	// unlisted holds the name of each call to a tool the run may not call, by call ID, until its result arrives.
	unlisted map[string]string
	// searches holds the query of each WebSearch call that waits for its result. The result records the search as one item.
	searches map[string]string
	// askedCh receives the ID of a question call whose request opened, and
	// asked holds it once the run ends for the answer. recorded tells the
	// bridge when the run recorded a call.
	askedCh  chan string
	asked    string
	recorded recorded
	// askMsg maps each question call that waits for its response to end to the ID of the response.
	askMsg map[string]string
	// read returns the bytes of an attachment of the turn.
	read func(key string) ([]byte, error)
	// continues reports a run of a turn whose input the CLI already took;
	// taken reports that this run gave the CLI the input.
	continues bool
	taken     bool
	stopped   bool

	// limits is the newest subscription snapshot of the run; the result
	// reports it with the usage of its call.
	limits  *protocol.SubscriptionUsage
	started bool
	// early holds the mirror frames that arrive before the init frame names
	// the session that they belong to.
	early   []envelope
	sendErr error
	tailErr error
	result  *envelope
	// pending is the assistant item that later frames of the same API
	// response join, until it holds a tool call.
	pending   *protocol.LogMessage
	pendingID string
	// spoke reports that the run recorded an assistant item.
	spoke bool
	// steer reports a steer input that waits for Sink.Steer. A resolution
	// run takes none and leaves the notification for the run after it: the
	// CLI would queue the input behind its own continuation and answer it in a
	// second result.
	steer bool
	// calls holds the tool calls of the main thread that wait for their
	// result. A steer input reaches the CLI only while one is in flight: the
	// CLI takes a queued message at the next tool result, and a message that
	// arrives in a final text reply starts a second turn after the result.
	calls map[string]bool
	// owed reports a steer input on stdin that no tool result has taken, and
	// waiting a run that reads on for the second result that the CLI owes.
	owed, waiting bool
	// cost is the total_cost_usd of the previous result of the run, which the
	// CLI reports as a running total.
	cost              float64
	mainModel         string
	lastCall          *usage
	compactPostTokens int64
	compact           string
	sawCompact        bool
	// live is the API response that the stream events of each thread are
	// streaming, by parent tool call ID.
	live map[string]*liveMessage
	// compacting reports a compaction that the CLI started and did not settle.
	compacting bool
}

func (r *run) cleanup() {
	if r.tools != nil {
		r.tools.Close()
	}
	for _, p := range append([]string{r.dir}, r.temps...) {
		if p != "" {
			_ = os.RemoveAll(p)
		}
	}
}

// drive sends the prompt and handles frames until the result, the end of
// stdout, or the end of ctx.
func (r *run) drive(ctx context.Context, req turn.Request) error {
	r.read = func(key string) ([]byte, error) { return req.Blob(ctx, key) }
	line, err := r.prompt(req)
	if err != nil {
		return err
	}
	r.sendErr = r.proc.Send(line)
	steered := req.Steered
	for {
		select {
		case id := <-r.askedCh:
			r.asked = id
			return errAsked
		case line, ok := <-r.proc.Lines():
			if !ok {
				if r.waiting {
					return errSteerUnanswered
				}
				return errExited
			}
			if err := r.frame(line); err != nil || r.result != nil && !r.waiting && !r.awaitSecond() {
				return err
			}
		case <-steered:
			r.steer = true
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		if r.steer && len(r.calls) > 0 {
			if err := r.steered(); err != nil {
				return err
			}
		}
	}
}

// awaitSecond reports that the CLI owes a second result. A steer input that
// no tool result took waits in the queue of the CLI, which runs it as the next
// turn after this result, so the run holds no result until the second one.
func (r *run) awaitSecond() bool {
	if !r.owed || r.result.IsError {
		return false
	}
	r.owed, r.waiting, r.result = false, true, nil
	return true
}

var errSteerUnanswered = errors.New("claudecode: the CLI exited before it answered a steer input")

var errExited = errors.New("claudecode: the CLI exited before its result")

// finish ends the process, maps the frames that it writes until it exits,
// saves the external session, and returns the turn error. A stopped turn
// keeps every item that the CLI wrote, and completes on a success result.
func (r *run) finish(ctx context.Context, err error) error {
	r.stopped = ctx.Err() != nil
	if r.asked != "" {
		err = nil
	}
	if err == nil && r.asked == "" {
		r.proc.CloseInput()
	} else if !r.stopped {
		r.proc.Interrupt()
	}
	exit := r.proc.Finish(grace, r.tail)
	r.settleCompaction(protocol.StatusCompactionFailed)
	if r.stopped {
		err = stopError(err, context.Cause(ctx), r.result)
	}
	err = r.outcome(errors.Join(err, r.tailErr, r.flush()), exit)
	if r.taken && err != nil && errors.Is(context.Cause(ctx), turn.ErrHandoff) {
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
	case r.asked != "":
		return nil
	}
	if res := r.result; res.IsError {
		failure := res.failure()
		err = fmt.Errorf("claudecode: the turn failed (%s): %s", res.Subtype, failure)
		if retryable(res.Subtype, failure) {
			err = fmt.Errorf("%w: %w", turn.ErrRetryable, err)
		}
	}
	return err
}

// save saves the external session. The saved state keeps only what changed.
func (r *run) save() error {
	head, err := r.mirror.Head()
	if err != nil {
		return err
	}
	return r.out.SaveState(stateKey, turn.Snapshot{Head: head, Entries: r.mirror.Entries})
}

// tail maps a frame that the CLI writes after its result or after the turn
// failed: only its transcript and its result. After a stop, it maps every
// frame but the stream events: the CLI still writes the items that it already
// ran, and a block that it did not finish arrives as one assistant frame.
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
	case (r.stopped || r.asked != "") && env.Type != "stream_event" || env.Type == "transcript_mirror":
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
	if !r.started && (env.Type == "assistant" || env.Type == "user" || env.Type == "result" && !env.IsError) {
		return fmt.Errorf("%w: %s frame before init", ErrToolsNotRestricted, env.Type)
	}
	if r.compact != "" && env.Type != "transcript_mirror" && env.Type != "stream_event" {
		if summary, err := r.compacted(env); err != nil || summary {
			return err
		}
	}
	switch env.Type {
	case "system":
		return r.system(env)
	case "assistant":
		return r.assistant(env)
	case "user":
		return r.toolResults(env)
	case "stream_event":
		return r.streamEvent(env)
	case "result":
		if r.placeholder(env) {
			return nil
		}
		r.result, r.waiting = &env, false
		clear(r.calls)
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
	if !r.started {
		r.early = append(r.early, env)
		return nil
	}
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
		reset := env.SessionID != "" && env.SessionID != r.mirror.SessionID
		if reset {
			r.mirror.SessionID, r.mirror.Path, r.mirror.Entries = env.SessionID, "", nil
		}
		early := r.early
		r.early = nil
		for _, e := range early {
			if err := r.mirror.Add(r.dir, e.FilePath, e.Entries); err != nil {
				return err
			}
		}
		if reset {
			return r.save()
		}
	case "status":
		switch {
		case env.Status == "compacting":
			r.compacting = true
			r.out.Status(protocol.StatusFrame{Status: protocol.StatusCompacting})
		case env.CompactResult != "" && env.CompactResult != "success":
			r.compacting = false
			r.out.Status(protocol.StatusFrame{Status: protocol.StatusCompactionFailed})
		}
	case "compact_boundary":
		r.settleCompaction(protocol.StatusRunning)
		r.sawCompact = true
		r.compact = "Claude Code compacted its context."
		if m := env.CompactMetadata; m != nil {
			r.compact = fmt.Sprintf("Claude Code compacted its context (%s, %d tokens before).", m.Trigger, m.PreTokens)
			if m.PostTokens > 0 {
				r.compactPostTokens = m.PostTokens
			}
		}
	}
	return nil
}

// mayCall reports whether the CLI may call the tool name. A restricted run
// calls only its allowed tools. Any run calls MCP tools. A call to any other
// tool waits for its result; see checkRan.
func (r *run) mayCall(name string) bool {
	if r.restricted {
		return r.allowed[name]
	}
	return strings.HasPrefix(name, "mcp__") || r.allowed[name]
}

// checkRan fails the turn when the result of a call to an unlisted tool is not
// the refusal of the CLI: an error result whose tool_use_error text is "No
// such tool available: <name>", ended by "." for a disabled built-in or by
// the end of the text for a name the CLI does not know, in a frame whose
// tool_use_result is a string. A tool that ran gives an object there, and any
// other wording fails closed.
func (r *run) checkRan(parts []protocol.LogPart, toolUseResult json.RawMessage) error {
	for _, p := range parts {
		name, ok := r.unlisted[p.CallID]
		if !ok {
			continue
		}
		delete(r.unlisted, p.CallID)
		if !refused(p, name, toolUseResult) {
			return fmt.Errorf("%w: tool %s ran", ErrToolsNotRestricted, name)
		}
	}
	return nil
}

func refused(p protocol.LogPart, name string, toolUseResult json.RawMessage) bool {
	var s string
	rest, ok := strings.CutPrefix(p.Text, "<tool_use_error>Error: No such tool available: "+name)
	return p.IsError && ok && (strings.HasPrefix(rest, ".") || rest == "</tool_use_error>") &&
		len(toolUseResult) > 0 && toolUseResult[0] == '"' && json.Unmarshal(toolUseResult, &s) == nil
}

func (r *run) checkTools(tools *[]string) error {
	if tools == nil {
		return fmt.Errorf("%w: the init frame lists no tools", ErrToolsNotRestricted)
	}
	for _, t := range *tools {
		if strings.HasPrefix(t, "mcp__") && !r.restricted {
			continue
		}
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
		isSummary = true
		if plain := plainSummary(s); plain != "" {
			summary = plain
		}
	}
	return isSummary, r.out.Compacted(summary)
}

// settleCompaction ends the compaction that the CLI started with one status
// frame, or sends none when the CLI started none.
func (r *run) settleCompaction(status protocol.Status) {
	if r.compacting {
		r.compacting = false
		r.out.Status(protocol.StatusFrame{Status: status})
	}
}

func (r *run) assistant(env envelope) error {
	m := decodeMessage(env.Message)
	if m.Usage != nil && env.ParentToolUseID == "" {
		r.lastCall = m.Usage
		r.compactPostTokens = 0
	}
	parts := assistantParts(m)
	if len(parts) == 0 {
		return nil
	}
	for _, p := range parts {
		if p.Type == protocol.LogPartToolCall && !r.mayCall(p.Name) {
			r.unlisted[p.CallID] = p.Name
		}
	}
	if parts = r.holdSearches(parts, env.ParentToolUseID); len(parts) == 0 {
		return r.flush()
	}
	if r.pending == nil || m.ID == "" || m.ID != r.pendingID || r.pending.ParentCallID != env.ParentToolUseID {
		if err := r.flush(); err != nil {
			return err
		}
		r.pending, r.pendingID = &protocol.LogMessage{Role: protocol.RoleAssistant, ParentCallID: env.ParentToolUseID}, m.ID
	}
	for i, p := range parts {
		switch p.Type {
		case protocol.LogPartText, protocol.LogPartReasoning:
			if text := r.unstreamed(env.ParentToolUseID, m.ID, p); text != "" {
				r.thread(env.ParentToolUseID).Delta(r.pendingID, turn.Delta{Type: p.Type, Text: text})
			}
		case protocol.LogPartToolCall:
			if name, ok := strings.CutPrefix(p.Name, mcpPrefix); ok && r.bridged[name] {
				parts[i].Name = name
			}
			r.names[p.CallID] = parts[i].Name
			if env.ParentToolUseID == "" {
				r.calls[p.CallID] = true
			}
		}
	}
	r.pending.Parts = append(r.pending.Parts, parts...)
	if m.ID == "" || slices.ContainsFunc(parts, func(p protocol.LogPart) bool { return p.Type == protocol.LogPartToolCall }) {
		err := r.flush()
		for _, p := range parts {
			if p.Type == protocol.LogPartToolCall && p.Name == turn.AskTool && env.ParentToolUseID == "" {
				r.askMsg[p.CallID] = m.ID
			}
		}
		r.releaseAsks()
		return err
	}
	return nil
}

func (r *run) toolResults(env envelope) error {
	parts := toolResults(decodeMessage(env.Message), r.names)
	if r.asked != "" {
		parts = slices.DeleteFunc(parts, func(p protocol.LogPart) bool { return p.CallID == r.asked })
	}
	if len(parts) == 0 {
		return nil
	}
	ran := r.checkRan(parts, env.ToolUseResult)
	if err := r.record(env, parts); err != nil {
		return err
	}
	return ran
}

// record stores the results of a frame, also when a built-in tool ran, so the
// log keeps every real result.
func (r *run) record(env envelope, parts []protocol.LogPart) error {
	if err := r.attachBlobs(parts); err != nil {
		return err
	}
	if env.ParentToolUseID == "" {
		r.owed = false
		for _, p := range parts {
			delete(r.calls, p.CallID)
		}
	}
	if err := r.flush(); err != nil {
		return err
	}
	parts, err := r.recordSearches(parts, env.ParentToolUseID, env.ToolUseResult)
	if err != nil || len(parts) == 0 {
		return err
	}
	return r.out.Item(protocol.LogMessage{Role: protocol.RoleTool, Parts: parts, ParentCallID: env.ParentToolUseID})
}

// flush records the pending assistant item.
func (r *run) flush() error {
	if r.pending == nil {
		return nil
	}
	m := *r.pending
	r.pending, r.pendingID, r.spoke = nil, "", true
	if err := r.thread(m.ParentCallID).Item(m); err != nil {
		return err
	}
	r.replay(m.ParentCallID)
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
			r.owed = r.owed || err == nil
		}
	}
	return err
}

// placeholder reports whether env is the empty zero-turn result that the
// CLI sends for a queued task notification. The real turn follows it. A
// compaction ends with the same shape and is final, and so is a result after
// the run recorded an assistant item.
func (r *run) placeholder(env envelope) bool {
	return !env.IsError && env.NumTurns != nil && *env.NumTurns == 0 && env.Result == "" &&
		r.pending == nil && !r.spoke && env.LocalCommand == "" && !r.sawCompact
}

// settle records the items and the telemetry of the result.
func (r *run) settle(env envelope) error {
	if err := r.flush(); err != nil {
		return err
	}
	r.telemetry(env)
	return nil
}

func (r *run) telemetry(env envelope) {
	cost := max(0, env.TotalCostUSD-r.cost)
	r.cost = env.TotalCostUSD
	t := turn.Telemetry{Usage: env.Usage.usage(), SubscriptionUsage: r.limits, CostUSD: &cost, Call: callMetrics(env)}
	if r.compactPostTokens > 0 {
		t.Context.Tokens = r.compactPostTokens
	} else {
		last := r.lastCall
		if last == nil {
			last = env.Usage
		}
		t.Context.Tokens = last.prompt()
	}
	t.Context.Window = env.ModelUsage[r.mainModel].ContextWindow
	t.Context.Source = stateKey
	r.out.Telemetry(t)
}

// callMetrics reads the timing of the result. A first-token time without a
// duration leaves the stream time at zero.
func callMetrics(env envelope) *turn.CallMetrics {
	ttft := time.Duration(env.TTFTMillis) * time.Millisecond
	return &turn.CallMetrics{TTFT: ttft, Stream: max(0, time.Duration(env.DurationMillis)*time.Millisecond-ttft)}
}

// keepBlobs remembers the blobs of the result of call id, for the result frame
// that the CLI prints after the bridge call.
func (r *run) keepBlobs(id string, blobs []protocol.Blob) {
	r.blobMu.Lock()
	defer r.blobMu.Unlock()
	if r.blobs == nil {
		r.blobs = map[string][]protocol.Blob{}
	}
	r.blobs[id] = blobs
}

// attachBlobs stores the blobs of each bridge result and adds their parts to
// the result part, so the log holds the image that the CLI saw.
func (r *run) attachBlobs(parts []protocol.LogPart) error {
	a, ok := r.out.(turn.Attacher)
	if !ok {
		return nil
	}
	r.blobMu.Lock()
	defer r.blobMu.Unlock()
	for i, p := range parts {
		for _, b := range r.blobs[p.CallID] {
			if len(b.Data) == 0 {
				continue
			}
			bp, err := a.Attach(b.MediaType, b.Data)
			if err != nil {
				return err
			}
			parts[i].Blobs = append(parts[i].Blobs, bp)
		}
		delete(r.blobs, p.CallID)
	}
	return nil
}
