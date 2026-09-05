// Tool calls run concurrently with serial and key barriers. Results join in call order. File keys do not cover Bash side effects or hard links.
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

	// Backfill preserves one result per call after cancellation or a panic.
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
	// is no window to race. Keep retention at this join unless reservation and minting become atomic.
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

// These are the three synthesized results the executor produces when a
// tool did not return a normal result: the call was refused after the
// turn was canceled, no execution path filled its slot, or the tool
// panicked. All three exist to hold the pairing invariant: a tool_use
// block with no tool_result wedges a session permanently (see
// docs/engine-request-cycle.md, NEP-5272).
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
// real gate for them. The turn is over, so the only thing still-starting
// work can add is a side effect nobody asked for.
//
// A canceled turn's results are NOT discarded, and an earlier version of
// this comment wrongly said they were. runToolCalls returns the
// synthesized results, runAgenticLoop appends the whole RoleTool message
// to DURABLE history, and they survive a resume. That makes the gate more
// important, not less: what lands in the log is a legible "not started"
// result for each refused call, instead of a side effect the operator
// aborted to prevent.
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
// calls. See the batching contract.
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
		if len(seg.calls) == 1 {
			// One-call fast path. A parallel segment of exactly one call
			// is the COMMON shape — a turn with a single tool call, or a
			// lone call between two Serial ones — and building a
			// goroutine, a WaitGroup, a semaphore channel and a keyChain
			// for it buys nothing: there is no sibling to run beside, and
			// no sibling to exclude. Running it inline is what the serial
			// branch above already does.
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
	// batching contract.
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

// toolBatchingSegment is the system-prompt segment that tells the model to
// put independent tool calls in ONE message, so this file's executor
// actually has a batch to run in parallel. Without it the executor is
// unclaimed capacity: a model that emits one call per turn never produces
// a batch wider than one, however high the cap is.
//
// It is gated on the session's REAL resolved concurrency, and returns ""
// at 1. A session running strictly sequentially — an operator who set the
// kill switch, or an embedder who set ToolConcurrency 1 — must not be told
// its calls run concurrently, because for that session they do not.
//
// The cap is rendered from s.toolConcurrency rather than hardcoded, so the
// number the model reads is the number the executor enforces.
//
// The second sentence is as load-bearing as the first: it stops the model
// batching calls whose arguments depend on an earlier call's result, which
// no amount of executor correctness can repair.
func (s *Session) toolBatchingSegment() string {
	if s.toolConcurrency <= 1 {
		return ""
	}
	return fmt.Sprintf("If you intend to call multiple tools and there are no "+
		"dependencies between the calls, make all of the independent calls in "+
		"the same message: harness runs one message's tool calls concurrently, "+
		"up to %d at a time. Otherwise you MUST wait for previous calls to "+
		"finish first to determine the dependent values.", s.toolConcurrency)
}
