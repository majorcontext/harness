// Parallel tool-call execution: one assistant message's batch of tool
// calls runs concurrently, bounded by a cap, instead of one at a time.
// Claude Code's own contract is the model: the model already treats every
// call inside one message as independent (that is the entire reason it
// batched them), so harness executes the batch that way too. Measured
// impact is 2-3x wall clock on a batched-read turn.
//
// # Audit: session-state assumptions runToolCall's callees make
//
// This section is Part A of the design: every piece of session state a
// built-in tool's Run touches, and whether it survives concurrent calls
// from one batch unchanged. Each finding is verified against source, not
// assumed.
//
//   - s.emit (-> Config.OnEvent): now called from several goroutines at
//     once for one batch (EventToolStart/EventToolEnd interleave across
//     calls). s.emit itself does nothing but stamp SessionID and invoke
//     the callback — no shared mutable state, so concurrent invocation is
//     safe AT THIS LAYER. The obligation moves to the callback: see
//     Config.OnEvent's doc comment (engine.go), which this feature updates
//     to say the engine MAY call it from several goroutines at once. Both
//     in-repo consumers already tolerate this: server.Publish/publishLive
//     (server/journal.go) route every event through s.mu-guarded
//     bookkeeping, and cmd/harness's newRunOnEventHandler already wraps
//     its callback in a mutex — TestRunOnEventHandlerSerializesConcurrentCallers
//     covers exactly this, pre-dating this feature (a `task` child's
//     background goroutine could already call the same OnEvent concurrently
//     with its parent's own turn). Neither needed a change for this PR.
//
//   - emitToolExecuteStart/emitToolExecuteEnd (-> Config.Hooks.Emit) and
//     the ToolExecuteBefore/ToolExecuteAfter/ExecuteTool hook dispatches:
//     plugin.Host.Emit enqueues onto a per-plugin-instance channel (safe
//     for concurrent senders) and a dedicated per-instance goroutine drains
//     it in RECEIPT order — see Host.Emit's own doc comment. Receipt order
//     across DIFFERENT call ids is whatever order concurrent callers
//     enqueued in (i.e., completion order for that hook type), same
//     acceptance as the mcp.tools_selected note below. Host.ExecuteTool and
//     the dispatchChain-based hooks each open their own request over the
//     shared conn, keyed by a fresh atomic request id (conn.call, protocol.go)
//     and serialized only at the write (conn.wmu) — safe for concurrent
//     calls, ordinary RPC client behavior.
//
//   - s.toolExecCount++ (engine.go): already under s.mu — verified at the
//     call site. No change needed.
//
//   - maybeRetainToolResult (toolresult.go): MUST run at the JOIN, in call
//     order, over the whole batch — not per-call. It is internally
//     s.mu-guarded, but two of its effects are only correct when called in
//     call order for one batch:
//     1. writeRetainedToolResult mints the next handle from
//     s.toolResultNextID and journals one durable record per
//     retention (toolresult.go). Concurrent retention would make
//     handle numbers (trh_N) and their journal order depend on
//     completion order instead of call order, an observable,
//     nondeterministic transcript.
//     2. The per-session retained-bytes ceiling check
//     (maybeRetainToolResult's `used+len(masked) > cap` branch) reads
//     s.toolResultBytes and compares it OUTSIDE the lock that later
//     writes it back (writeRetainedToolResult's own separate
//     acquisition) — a check-then-act split across two s.mu sections.
//     Two concurrent retentions can each observe the ceiling as not
//     yet crossed and both proceed, when only one should have.
//     Running retention at the join, sequentially in call order, closes
//     both: handle numbering and journal order become call-order again,
//     and the ceiling check-then-act is never concurrent with itself.
//     This changes nothing observable about EventToolEnd: that event
//     already carries the PRE-retention output today (retention happens
//     in the old runToolCalls, one level above runToolCall — see git
//     history), so moving retention's OWN call site later, to the join,
//     is not a new event-ordering change. An intra-batch handle
//     dependency (one call's arguments naming a handle another call in
//     the SAME batch is about to mint) is structurally impossible: the
//     model can only ever learn a handle from a PREVIOUS turn's tool
//     result, which by definition is not part of the batch currently
//     executing.
//
//   - markMCPToolsSelected (mcp_lazy.go): already mutates and journals
//     under s.mu — verified at the call site. Under a parallel batch, the
//     only new nondeterminism is the ORDER of mcp.tools_selected records
//     across sibling calls in one batch, which becomes completion-order
//     instead of call-order. Accepted: each record names its own tool by
//     value (there is no ordering-sensitive accumulation — see
//     markMCPToolsSelected's `if s.mcpSelected[name] { continue }` check),
//     so two records landing in either relative order describe the exact
//     same eventual selected set. This is NOT the same class of bug as
//     the retention ceiling above, where relative order changes the
//     OUTCOME (which call wins the ceiling), not just the record order.
//
//   - s.tools (the built-in tool registry map): written only by
//     newSession, LoadSession, and SessionManager's adopt/spawn paths
//     (store.go, session_manager.go) — every one of those runs BEFORE a
//     session is exposed to any concurrent turn, never during one.
//     Verified: no write site is reachable from runToolCall or anything
//     it calls. Concurrent READS during a batch (executeTool's
//     `s.tools[tc.Name]` lookup) are therefore safe with no lock — an
//     unsynchronized read of a map nobody is writing.
//
//   - s.cfg.WorkDir / s.resolvePath: WorkDir is set once at construction
//     and never mutated; resolvePath (filetools.go) is a pure function of
//     it plus the call's own argument. Safe to call concurrently.
//
//   - s.cfg.MCP.CallTool (mcp.go): CallTool takes MCPManager's RWMutex only
//     for the binding lookup (RLock) and delegates the actual call to
//     callTool, which opens its own request over the connected client —
//     ordinary concurrent-safe client usage. Safe for concurrent calls.
//
// # Design
//
// A batch is one assistant message's ToolCall parts, in order. splitBatch
// walks them and cuts a new segment at each Serial call (its own
// single-element segment) and at each run of non-Serial calls (a parallel
// segment). Segments run in order; a segment completes fully — every call
// in it has a result — before the next segment starts. This is the
// "barrier" semantic: everything before a Serial call has already
// finished, and nothing after it starts until it returns.
//
// Within one parallel segment, up to s.toolConcurrency calls run at once,
// via a bounded worker pool. Results land in a slice indexed by the call's
// position in the whole batch (not just its segment), so the join can
// walk the batch once, in order, regardless of which segment or worker
// produced which result.
//
// # Per-key mutual exclusion invariant
//
// Two calls in one batch that share a non-empty Key must never run
// concurrently, and must run in CALL order (the first one queued must be
// the first one to acquire the key) — a plain sync.Mutex does not
// guarantee the second property, since Go's Mutex is not FIFO under
// contention. keyChain implements an explicit hand-off baton instead: the
// Nth call for a key waits on a channel the (N-1)th call closes when it
// finishes, and creates the channel the (N+1)th call will wait on before
// releasing its own turn. This is built and wired up FRONT-TO-BACK, before
// any worker goroutine starts, precisely so waiting for a predecessor's
// baton can never itself block on the worker pool (see below).
//
// Invariant, written down before implementation per AGENTS.md: a same-key
// call's wait for its predecessor must NEVER depend on that predecessor
// having already acquired a worker-pool slot. If the wait were expressed
// as "block until the predecessor's goroutine starts running", and the
// predecessor is itself still queued behind the concurrency cap, a
// same-key successor that already holds a slot would block forever inside
// it — starving the pool of the one slot the predecessor needs to make
// progress, a classic self-deadlock. The fix: keyChain hands out every
// call's baton channel synchronously, on the submitting goroutine, for the
// WHOLE segment before any worker begins running calls. A worker that
// dequeues a call only waits on a channel that already exists and will be
// closed by whichever call — running now, or still queued — owns it; it
// never waits on a goroutine that has not been scheduled yet to CREATE
// that channel. Waiting for a predecessor's completion therefore never
// competes with that predecessor for a pool slot: the predecessor, once it
// does get a slot, runs and closes the channel independent of who else is
// waiting on it.
//
// # Residuals of the per-file key
//
// filePathKey covers read_file, write_file and edit_file. Two tool
// shapes sit OUTSIDE that namespace, and an operator relying on the
// same-file guarantee should know both.
//
//  1. bash. bashTool (bash.go) sets neither Key nor Serial, and it runs
//     arbitrary shell: "echo x > f.txt" and "cat f.txt" touch files the
//     engine cannot see. A batch that pairs a bash write with an
//     edit_file on the same path, or two bash calls on one file, races on
//     disk. A bash call's file targets are not statically knowable, so
//     keying it generally is not possible, and keying it pessimistically
//     (one global bash key) would serialize the most common parallel
//     workload there is. bash stays parallel by design: the model owns
//     batching judgment, which is the same contract Claude Code ships.
//     The strictly-sequential default never exposed this, so it IS a
//     behavior change for the default configuration, and
//     HARNESS_SEQUENTIAL_TOOLS=1 is the operator's answer for a workload
//     that cannot tolerate it.
//  2. A symlink or hard link reaching one inode under two paths keys as
//     two resources. See filePathKey for why resolving that is not worth
//     a syscall on the batch's hot path.
//
// # Cancellation and the orphan-result invariant
//
// ctx cancellation (an aborted turn) cancels every in-flight call's own
// context, but every call still yields exactly one ToolResult — the
// NEP-5272 invariant (AGENTS.md's "empty tool result" rule, and the
// orphan tool_use rule generally) holds regardless of how the batch ends.
// A call whose ctx is already cancelled before it starts still runs
// (executeTool/runToolCall are unchanged; a cancelled context is not a
// license to skip a call, only a signal the call's own logic may check —
// bash, for instance, already turns ctx.Err() into a captured-output
// result, not a skip). This mirrors runToolCall's pre-existing contract:
// this package does not add a new cancellation check that could produce
// zero results for a call.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/majorcontext/harness/message"
)

// defaultToolConcurrency is Config.ToolConcurrency's zero-value default:
// the cap on how many of one batch's tool calls run at once when the
// operator has not set an explicit value. Chosen so a wide but ordinary
// batch (a handful of reads, a few greps) runs fully in parallel while a
// pathological one (20 globs) cannot fork-bomb a small (2 vCPU) box.
const defaultToolConcurrency = 8

// resolveToolConcurrency implements Config.ToolConcurrency's precedence:
// explicit value wins (clamped to a sane floor), zero (unset) is the
// package default. See Config.ToolConcurrency's doc comment for the full
// contract; this mirrors resolveContextWindow's shape (context_window.go).
func resolveToolConcurrency(explicit int) int {
	switch {
	case explicit == 0:
		return defaultToolConcurrency
	case explicit < 0:
		// A negative value is not "unlimited" — clamp to the strictly
		// sequential floor rather than silently misinterpreting the
		// operator's intent.
		return 1
	default:
		return explicit
	}
}

// runToolBatch is runToolCalls' actual implementation (see engine.go),
// split out here to keep this package's concurrency logic in one file.
// It executes every ToolCall part of asst and returns their ToolResult
// parts, one per call, in CALL order — regardless of completion order,
// concurrency, or partial failure.
func (s *Session) runToolBatch(ctx context.Context, asst *message.Message) message.Parts {
	calls := toolCallsOf(asst)
	if len(calls) == 0 {
		return nil
	}

	outputs := make([]message.Parts, len(calls))
	errs := make([]bool, len(calls))
	// done is the ONE-RESULT-PER-CALL ledger. A result slot exists for
	// every call before anything is scheduled, and every execution path
	// marks its own slot. The backfill below then turns "no path marked
	// this slot" into a real error result instead of an orphan tool_use.
	// outputs[i] alone cannot serve as the ledger: a tool may legitimately
	// return nil parts.
	done := make([]bool, len(calls))

	if s.toolConcurrency <= 1 {
		// Sequential mode: one call at a time, in call order. A Serial
		// tool's barrier is a no-op here since nothing ever runs beside
		// it, and per-key exclusion is likewise moot with one call in
		// flight at a time.
		//
		// This is the pre-parallel ORDER, not the pre-parallel behavior in
		// every respect. Two guarantees this file adds apply here too, by
		// design: runOneGuarded turns a panicking tool into one error
		// result instead of killing the process, and admitAndRun refuses
		// to start a call after the turn is canceled, where the old loop
		// ran every remaining call unconditionally. An operator who sets
		// ToolConcurrency 1 to escape concurrency gets exactly that —
		// serial execution — not a revert of the whole change.
		for i, tc := range calls {
			outputs[i], errs[i] = s.runOneGuarded(ctx, tc)
			done[i] = true
		}
	} else {
		s.runToolBatchParallel(ctx, calls, outputs, errs, done)
	}

	// Backfill: no call may leave this function without a result. See
	// "Cancellation and the orphan-result invariant" in the package doc
	// comment. Nothing reaches this loop today — every path above marks
	// its slot — and that is the point: it is the structural guarantee,
	// not a live code path.
	for i := range calls {
		if !done[i] {
			outputs[i] = message.Parts{&message.Text{Text: toolCallNoResultText}}
			errs[i] = true
		}
	}

	// The join. This is retention's single call site
	// (maybeRetainToolResult, toolresult.go): an oversized TEXT result is
	// swapped for a preview plus a trh_N handle HERE, before the
	// ToolResult is built and long before Session.append,
	// message.NormalizeForWire, or any transcoder sees it. That placement
	// is why tool-result handles need no wire-format change at all — every
	// downstream layer still sees an ordinary ToolResult carrying ordinary
	// Text parts. Retention runs on the post-hook output (runToolCall
	// already applied ToolExecuteAfter), so a plugin that rewrites or
	// enlarges a result has its final bytes measured, not the tool's
	// originals. It is a total no-op when retention is disabled or the
	// result is within the limit.
	//
	// Running it here, on the join goroutine, in call order, is also what
	// keeps maybeRetainToolResult SINGLE-THREADED. That matters beyond
	// handle numbering: its per-session retained-bytes ceiling is a
	// check-then-act split across two separate s.mu sections (it reads
	// s.toolResultBytes, unlocks, writes the sidecar, then adds the bytes
	// back under a second acquisition). Concurrent callers could each
	// observe the ceiling as uncrossed and all proceed, overshooting the
	// configured cap by up to the concurrency factor. The join removes the
	// concurrency instead of re-locking the ceiling: with one caller there
	// is no window to race. Do NOT move retention back inside the worker
	// without first making that reserve-and-mint one atomic section.
	results := make(message.Parts, len(calls))
	for i, tc := range calls {
		results[i] = &message.ToolResult{
			CallID:  tc.CallID,
			Content: s.maybeRetainToolResult(tc.Name, outputs[i]),
			IsError: errs[i],
		}
	}
	return results
}

// toolCallCanceledText and toolCallNoResultText are the two synthesized
// results the executor can produce without the tool running at all. Both
// exist to hold the pairing invariant: a tool_use block with no
// tool_result wedges a session permanently (AGENTS.md, NEP-5272).
const (
	toolCallCanceledText = "tool call not started: the turn was canceled"
	toolCallNoResultText = "tool call produced no result"
	toolCallPanicText    = "tool call failed: the tool panicked"
)

// admitAndRun is the ONE admission gate every call passes through. A call
// admitted while ctx is already canceled does not run: it returns a
// synthesized canceled result instead.
//
// This deliberately CHANGES the pre-parallel behavior, which ran every
// remaining call after an abort. Two reasons. A batch has calls queued
// behind the concurrency cap, so an abort that arrives early would
// otherwise keep starting fresh work for as long as the batch is wide.
// And several built-ins commit their side effect without consulting ctx
// at all — write_file writes the file — so "the tool decides" is not a
// real gate for them. The turn is over; a canceled turn's results are
// discarded anyway, so the only thing still running work can produce is
// an unwanted side effect.
//
// A call ALREADY RUNNING when the abort lands is not interrupted here: it
// owns its own ctx and returns whatever it returns. This gate governs
// admission only.
//
// A refused call emits NO tool events. Both EventToolStart and
// EventToolEnd fire inside runToolCall (engine.go), which this gate
// short-circuits, so an aborted batch's refused calls appear in the
// transcript as tool_result parts with no matching event pair. That is
// deliberate — the events describe an execution that never happened — and
// it is safe because an aborted turn's event stream is already incomplete
// by definition. The transcript, which the provider validates, still
// pairs every tool_use with a tool_result.
func (s *Session) admitAndRun(ctx context.Context, tc *message.ToolCall) (message.Parts, bool) {
	if ctx.Err() != nil {
		return message.Parts{&message.Text{Text: toolCallCanceledText}}, true
	}
	return s.runToolCall(ctx, tc)
}

// runOneGuarded is the single execution wrapper every path uses. It turns a
// PANIC inside a tool, or inside a hook the tool's dispatch calls, into one
// ordinary error result.
//
// Without it the one-result-per-call guarantee is not true. A panic in a
// worker goroutine cannot be recovered by the join, so it takes the whole
// process down and the batch's other results die with it — and the
// assistant message's tool_use blocks are already in history, so the next
// load of that session meets unanswered tool calls. A recovered panic
// keeps the session honest instead: the model gets a real error result for
// the call that failed, and its siblings still return their own.
//
// This also changes sequential mode, which previously let a tool panic
// unwind through Prompt. That is deliberate: the guarantee must not depend
// on which execution mode a session runs in.
func (s *Session) runOneGuarded(ctx context.Context, tc *message.ToolCall) (out message.Parts, isErr bool) {
	defer func() {
		if r := recover(); r != nil {
			out = message.Parts{&message.Text{Text: fmt.Sprintf("%s: %v", toolCallPanicText, r)}}
			isErr = true
		}
	}()
	return s.admitAndRun(ctx, tc)
}

// toolCallsOf extracts asst's ToolCall parts in order.
func toolCallsOf(asst *message.Message) []*message.ToolCall {
	var calls []*message.ToolCall
	for _, p := range asst.Parts {
		if tc, ok := p.(*message.ToolCall); ok {
			calls = append(calls, tc)
		}
	}
	return calls
}

// batchSegment is one contiguous run of calls executed as a unit: either a
// single Serial call, or a run of non-Serial calls that execute in
// parallel. idx holds each call's ORIGINAL index into the whole batch, so
// results can be written back to the right slot regardless of segment
// boundaries.
type batchSegment struct {
	idx    []int
	calls  []*message.ToolCall
	serial bool
}

// splitBatch walks calls in order and cuts a new segment at each Serial
// call — its own one-element segment — and at each run of non-Serial
// calls. See the package doc comment's "Design" section.
func (s *Session) splitBatch(calls []*message.ToolCall) []batchSegment {
	var segs []batchSegment
	var cur batchSegment
	flush := func() {
		if len(cur.calls) > 0 {
			segs = append(segs, cur)
			cur = batchSegment{}
		}
	}
	for i, tc := range calls {
		if s.toolIsSerial(tc.Name) {
			flush()
			segs = append(segs, batchSegment{idx: []int{i}, calls: []*message.ToolCall{tc}, serial: true})
			continue
		}
		cur.idx = append(cur.idx, i)
		cur.calls = append(cur.calls, tc)
	}
	flush()
	return segs
}

// toolIsSerial reports whether name names a built-in Tool with Serial set.
// A plugin or MCP tool is never Serial — only a built-in Tool carries the
// flag (see the Tool struct's doc comment).
func (s *Session) toolIsSerial(name string) bool {
	t, ok := s.tools[name]
	return ok && t.Serial
}

// toolKey computes name's resource key for one call's args, or "" if the
// tool has no Key func. See the Tool struct's Key field doc comment.
func (s *Session) toolKey(name string, args json.RawMessage) (key string) {
	t, ok := s.tools[name]
	if !ok || t.Key == nil {
		return ""
	}
	// A panicking Key runs on the SUBMITTING goroutine, before any result
	// slot is filled, so it would take down the batch with no results at
	// all. Fall back to one shared per-tool key instead: conservative,
	// because every call whose key could not be computed then serializes
	// with every other one, exactly like filePathKey's own unparsed
	// fallback. Never fall back to "" — that would silently drop the
	// exclusion the tool asked for.
	defer func() {
		if r := recover(); r != nil {
			key = "panicking-key:" + name
		}
	}()
	return t.Key(s, args)
}

// runToolBatchParallel executes calls' segments in order, filling outputs/
// errs by original batch index. Caller has already checked
// s.toolConcurrency > 1.
func (s *Session) runToolBatchParallel(ctx context.Context, calls []*message.ToolCall, outputs []message.Parts, errs []bool, done []bool) {
	for _, seg := range s.splitBatch(calls) {
		if seg.serial {
			i := seg.idx[0]
			outputs[i], errs[i] = s.runOneGuarded(ctx, seg.calls[0])
			done[i] = true
			continue
		}
		s.runParallelSegment(ctx, seg, outputs, errs, done)
	}
}

// keyChain is the per-key hand-off chain for one segment: the channel a
// waiting call blocks on, closed by its predecessor when done, and the
// channel the NEXT same-key call will wait on in turn.
// It carries no lock. wait is called only from runParallelSegment's setup
// loop, which runs entirely on the submitting goroutine before any worker
// starts; a worker only closes its own channel and reads its own
// predecessor. A mutex here would imply a concurrency contract that does
// not exist. If a future change ever calls wait from a worker, add the
// lock back with it.
type keyChain struct {
	tail map[string]chan struct{}
}

// wait registers this call as the next holder of key and returns a
// release func to call when the work is done. It returns immediately
// (nil predecessor channel) for the first call on a key. See the package
// doc comment's "Per-key mutual exclusion invariant" for why this hand-off
// is built synchronously, before any worker runs.
func (c *keyChain) wait(key string) (predecessor <-chan struct{}, release func()) {
	predecessor = c.tail[key]
	mine := make(chan struct{})
	if c.tail == nil {
		c.tail = make(map[string]chan struct{})
	}
	c.tail[key] = mine
	return predecessor, func() { close(mine) }
}

// runParallelSegment runs one non-Serial segment's calls with up to
// s.toolConcurrency in flight, honoring per-key exclusion. Every call gets
// exactly one result, in outputs/errs at its ORIGINAL batch index, even if
// ctx is already cancelled.
func (s *Session) runParallelSegment(ctx context.Context, seg batchSegment, outputs []message.Parts, errs []bool, done []bool) {
	// Baton hand-off is wired up front, on THIS goroutine, for every call
	// in the segment before any worker starts — see the invariant in the
	// package doc comment.
	var chain keyChain
	type job struct {
		idx         int
		tc          *message.ToolCall
		key         string
		predecessor <-chan struct{}
		release     func()
	}
	jobs := make([]job, len(seg.calls))
	for i, tc := range seg.calls {
		key := s.toolKey(tc.Name, tc.Arguments)
		var pred <-chan struct{}
		var rel func()
		if key != "" {
			pred, rel = chain.wait(key)
		}
		jobs[i] = job{idx: seg.idx[i], tc: tc, key: key, predecessor: pred, release: rel}
	}

	// The pool bounds EXECUTION, not goroutine count. Each job gets its
	// goroutine immediately; that goroutine waits for its baton FIRST and
	// only then takes a pool slot.
	//
	// The order matters, and an earlier cut had it the other way round
	// (slot taken on the submitting goroutine, baton awaited inside). That
	// shape lets a same-key waiter sit on a slot it cannot use while an
	// unrelated later call is refused admission — head-of-line blocking
	// that scales with how many same-key calls a batch carries. It could
	// not deadlock, because a predecessor is always submitted earlier and
	// so always holds a slot before its successor is even created, but a
	// slot held by a goroutine that is only waiting is pure waste. Waiting
	// for the baton before admission removes the whole class.
	//
	// A batch is bounded by one assistant message, so the goroutine count
	// is bounded too, and a parked goroutine costs a few kilobytes.
	slots := min(s.toolConcurrency, len(jobs))
	sem := make(chan struct{}, slots)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			// Release the baton on the way out, whatever happens inside:
			// a same-key successor must never wait forever on a
			// predecessor that died. Each call closes only its OWN
			// channel, exactly once.
			if j.release != nil {
				defer j.release()
			}
			if j.predecessor != nil {
				<-j.predecessor
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			outputs[j.idx], errs[j.idx] = s.runOneGuarded(ctx, j.tc)
			done[j.idx] = true
		}(j)
	}
	wg.Wait()
}
