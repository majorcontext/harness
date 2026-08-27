package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// TestAdvCapQueuesTheNinthUntilASlotFrees proves the cap is a real
// admission gate, not just a goroutine-count hint: with 12 calls and a cap
// of 8, exactly 8 run, and the 9th starts only once one of the first 8
// returns.
//
// synctest.Wait() is what makes this deterministic — it returns only when
// every other goroutine in the bubble is durably blocked, so "no 9th call
// has started" is an observation, not a race with a sleep.
func TestAdvCapQueuesTheNinthUntilASlotFrees(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n, cap = 12, 8
		var mu sync.Mutex
		inflight, maxInflight, entered := 0, 0, 0
		tokens := make(chan struct{}) // one send releases exactly one call

		s := NewSession(Config{ToolConcurrency: cap})
		s.tools["slow"] = batchTool("slow", func(context.Context, string) {
			mu.Lock()
			inflight++
			entered++
			if inflight > maxInflight {
				maxInflight = inflight
			}
			mu.Unlock()
			<-tokens
			mu.Lock()
			inflight--
			mu.Unlock()
		})

		calls := make([]*message.ToolCall, n)
		for i := range calls {
			calls[i] = batchCall("slow", fmt.Sprintf("c%d", i))
		}

		var results message.Parts
		done := make(chan struct{})
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
			close(done)
		}()

		synctest.Wait()
		mu.Lock()
		gotEntered, gotInflight := entered, inflight
		mu.Unlock()
		if gotEntered != cap || gotInflight != cap {
			t.Fatalf("with the batch stalled: entered=%d inflight=%d, want %d/%d (the cap must admit exactly %d)",
				gotEntered, gotInflight, cap, cap, cap)
		}

		// Free exactly one slot. The 9th call must now start — and only
		// the 9th.
		tokens <- struct{}{}
		synctest.Wait()
		mu.Lock()
		gotEntered, gotInflight = entered, inflight
		mu.Unlock()
		if gotEntered != cap+1 {
			t.Errorf("after freeing one slot, entered=%d, want %d (the 9th call must start, and no more)", gotEntered, cap+1)
		}
		if gotInflight != cap {
			t.Errorf("after freeing one slot, inflight=%d, want %d (still exactly at the cap)", gotInflight, cap)
		}

		for i := 0; i < n-1; i++ {
			tokens <- struct{}{}
		}
		<-done
		wantOrder(t, results, calls...)
		if maxInflight != cap {
			t.Errorf("peak concurrency = %d, want exactly %d", maxInflight, cap)
		}
	})
}

// TestAdvPeakConcurrencyMatchesTheCap sweeps cap settings, including the
// sequential floor the HARNESS_SEQUENTIAL_TOOLS=1 kill switch resolves to.
func TestAdvPeakConcurrencyMatchesTheCap(t *testing.T) {
	for _, tc := range []struct{ n, cap, want int }{
		{6, 1, 1}, // kill switch: strictly one at a time
		{6, 2, 2},
		{12, 8, 8}, // the default cap
		{3, 8, 3},  // fewer calls than the cap
		{6, -1, 1}, // negative clamps to sequential, never "unlimited"
	} {
		t.Run(fmt.Sprintf("n%d_cap%d", tc.n, tc.cap), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				inflight, maxInflight := 0, 0
				var order []string

				s := NewSession(Config{ToolConcurrency: tc.cap})
				s.tools["t"] = batchTool("t", func(_ context.Context, call string) {
					mu.Lock()
					inflight++
					if inflight > maxInflight {
						maxInflight = inflight
					}
					order = append(order, call)
					mu.Unlock()
					time.Sleep(time.Second)
					mu.Lock()
					inflight--
					mu.Unlock()
				})

				calls := make([]*message.ToolCall, tc.n)
				for i := range calls {
					calls[i] = batchCall("t", fmt.Sprintf("c%d", i))
				}
				results := s.runToolCalls(context.Background(), asstWith(calls...))
				wantOrder(t, results, calls...)

				if maxInflight != tc.want {
					t.Errorf("peak concurrency = %d, want %d", maxInflight, tc.want)
				}
				if tc.want == 1 {
					for i, c := range order {
						if c != fmt.Sprintf("c%d", i) {
							t.Errorf("sequential mode ran %v, want strict call order", order)
							break
						}
					}
				}
			})
		})
	}
}

// ---- Finding 4: path aliasing ----

// TestAdvPathAliasesCollapseToOneKey checks every spelling that can reach
// one file through the filesystem's own name resolution. Each must produce
// the SAME key, or two calls the executor believes are unrelated race on
// one file.
func TestAdvPathAliasesCollapseToOneKey(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(real, "f.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(real, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(real, "sub")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	dirlink := filepath.Join(real, "dirlink")
	if err := os.Symlink(subdir, dirlink); err != nil {
		t.Fatal(err)
	}

	s := NewSession(Config{WorkDir: real})
	key := func(path string) string {
		return filePathKey(s, json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)))
	}

	want := key(target)
	for _, spelling := range []struct{ name, path string }{
		{"relative", "f.txt"},
		{"absolute", target},
		{"dot-dot", filepath.Join(real, "sub", "..", "f.txt")},
		{"dot-slash", "./f.txt"},
		{"symlink to the file", link},
		{"redundant separators", real + "//f.txt"},
	} {
		if got := key(spelling.path); got != want {
			t.Errorf("%s: key(%q) = %q, want %q — an alias the executor would let race",
				spelling.name, spelling.path, got, want)
		}
	}

	// A not-yet-created file inside a SYMLINKED DIRECTORY: write_file's
	// common shape, where the full path cannot resolve because the leaf
	// does not exist yet.
	if a, b := key(filepath.Join(dirlink, "new.txt")), key(filepath.Join(subdir, "new.txt")); a != b {
		t.Errorf("symlinked-dir alias for a new file: %q != %q", a, b)
	}
}

// TestAdvHardLinkAliasIsNotCovered pins the ONE aliasing residual
// canonicalFileKeyPath documents (filetools.go): two hard links to one
// inode have no link to follow, so they take two keys and their calls run
// concurrently on one file.
//
// This test asserts the CURRENT, deliberately-unclosed behavior. It is a
// residual pin, not an approval: if it ever starts failing, someone closed
// the gap and canonicalFileKeyPath's comment should change with it. See
// that comment for why the fix is only partial (a not-yet-created
// write_file target has no inode to key on) rather than merely expensive.
func TestAdvHardLinkAliasIsNotCovered(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, "b.txt")
	if err := os.Link(a, b); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}

	s := NewSession(Config{WorkDir: dir})
	ka := filePathKey(s, json.RawMessage(fmt.Sprintf(`{"path":%q}`, a)))
	kb := filePathKey(s, json.RawMessage(fmt.Sprintf(`{"path":%q}`, b)))
	if ka == kb {
		t.Fatalf("hard-link aliases now share key %q — the documented residual is CLOSED; update canonicalFileKeyPath's docs", ka)
	}
	t.Logf("documented residual confirmed: %q != %q (same inode, two keys, calls run concurrently)", ka, kb)
}

// ---- Finding 4 continued: same-path file tools actually serialize ----

// TestAdvSamePathWritesSerializeWithoutCorruption runs two write_file calls
// at one path in one batch. They must serialize in call order and leave the
// SECOND call's content intact — never a byte-level interleave of the two.
func TestAdvSamePathWritesSerializeWithoutCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	first := strings.Repeat("A", 256*1024)
	second := strings.Repeat("B", 256*1024)

	s := NewSession(Config{WorkDir: dir, ToolConcurrency: 8})
	c1 := &message.ToolCall{CallID: "w1", Name: "write_file",
		Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":%q}`, path, first))}
	c2 := &message.ToolCall{CallID: "w2", Name: "write_file",
		Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":%q}`, path, second))}

	results := s.runToolCalls(context.Background(), asstWith(c1, c2))
	wantOrder(t, results, c1, c2)
	for i, p := range results {
		if tr := p.(*message.ToolResult); tr.IsError {
			t.Fatalf("write %d errored: %v", i+1, partsText(tr.Content))
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != second {
		if string(got) == first {
			t.Errorf("file holds the FIRST write — calls completed out of call order")
		} else {
			t.Errorf("file is neither write's content (%d bytes) — the two writes INTERLEAVED", len(got))
		}
	}
}

// TestAdvSamePathReadThenWriteHonorsTheGuardInCallOrder is the interaction
// between per-path keying and the read-before-overwrite guard. read_file
// and write_file share one key namespace, so a read placed BEFORE a write
// in the same batch must run first and authorize it. If the two raced, the
// write would intermittently be refused.
func TestAdvSamePathReadThenWriteHonorsTheGuardInCallOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewSession(Config{WorkDir: dir, ToolConcurrency: 8})
	read := &message.ToolCall{CallID: "r1", Name: "read_file",
		Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))}
	write := &message.ToolCall{CallID: "w1", Name: "write_file",
		Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"replaced"}`, path))}

	results := s.runToolCalls(context.Background(), asstWith(read, write))
	wantOrder(t, results, read, write)
	if tr := results[1].(*message.ToolResult); tr.IsError {
		t.Fatalf("write refused despite an in-batch read that precedes it: %v", partsText(tr.Content))
	}
	got, _ := os.ReadFile(path)
	if string(got) != "replaced" {
		t.Errorf("file = %q, want %q", got, "replaced")
	}
}

// TestAdvUnreadOverwriteStillRefusedUnderParallel proves parallelism did
// not punch a hole in the read-before-overwrite guard: an existing file
// this session never read is still protected, however many callers race
// for it.
func TestAdvUnreadOverwriteStillRefusedUnderParallel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "precious.txt")
	if err := os.WriteFile(path, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewSession(Config{WorkDir: dir, ToolConcurrency: 8})
	var calls []*message.ToolCall
	for i := 0; i < 6; i++ {
		calls = append(calls, &message.ToolCall{
			CallID:    fmt.Sprintf("w%d", i),
			Name:      "write_file",
			Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"clobbered-%d"}`, path, i)),
		})
	}
	results := s.runToolCalls(context.Background(), asstWith(calls...))
	wantOrder(t, results, calls...)
	for i, p := range results {
		if tr := p.(*message.ToolResult); !tr.IsError {
			t.Errorf("write %d SUCCEEDED on an unread existing file — the guard was bypassed", i)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "precious" {
		t.Errorf("file was modified to %q despite every write being refused", got)
	}
}

// partsText joins the Text parts of a result, for error messages.
func partsText(parts message.Parts) string {
	var b strings.Builder
	for _, p := range parts {
		if txt, ok := p.(*message.Text); ok {
			b.WriteString(txt.Text)
		}
	}
	return b.String()
}

// ---- Finding 2: aggregate result size ----

// TestAdvAggregateBatchBytesAreIdenticalParallelVsSequential measures the
// TOTAL inline bytes a wide batch of oversized results puts into the
// request, in both execution modes. The question the finding asks is
// whether parallel returns can push more past the per-call retention limit
// than the sequential path allowed. The answer must be that the two are
// byte-identical: retention runs at the JOIN, single-threaded, in call
// order, in both modes.
//
// It also reports the aggregate in absolute terms, because "each call is
// capped" and "the batch is capped" are different claims.
func TestAdvAggregateBatchBytesAreIdenticalParallelVsSequential(t *testing.T) {
	const n = 8
	const each = 400_000
	const inline = 2_000

	measure := func(concurrency int) (total int, retained int) {
		dir := t.TempDir()
		s := NewSession(Config{
			SessionDir:            dir,
			ToolResultInlineBytes: inline,
			ToolConcurrency:       concurrency,
		})
		s.tools["big"] = batchTool("big", nil)
		s.tools["big"] = Tool{
			Def: s.tools["big"].Def,
			Run: func(_ context.Context, _ *Session, args json.RawMessage) (message.Parts, error) {
				var in struct {
					Call string `json:"call"`
				}
				_ = json.Unmarshal(args, &in)
				return message.Parts{&message.Text{Text: in.Call + strings.Repeat("z", each)}}, nil
			},
		}
		calls := make([]*message.ToolCall, n)
		for i := range calls {
			calls[i] = batchCall("big", fmt.Sprintf("c%d", i))
		}
		results := s.runToolCalls(context.Background(), asstWith(calls...))
		wantOrder(t, results, calls...)
		for _, p := range results {
			total += len(partsText(p.(*message.ToolResult).Content))
		}
		s.mu.Lock()
		retained = len(s.toolResults)
		s.mu.Unlock()
		return total, retained
	}

	par, parHandles := measure(8)
	seq, seqHandles := measure(1)

	raw := n * each
	t.Logf("raw tool output:      %d bytes (%d calls x %d)", raw, n, each)
	t.Logf("parallel (cap 8):     %d bytes inline, %d handles retained", par, parHandles)
	t.Logf("sequential (cap 1):   %d bytes inline, %d handles retained", seq, seqHandles)
	t.Logf("per-call inline limit: %d; aggregate ceiling: NONE (n x limit grows linearly)", inline)

	if par != seq {
		t.Errorf("parallel put %d bytes in the request but sequential put %d — parallelism CHANGED the aggregate", par, seq)
	}
	if parHandles != seqHandles {
		t.Errorf("parallel retained %d handles, sequential %d", parHandles, seqHandles)
	}
}

// ---- Finding 8: cancellation ----

// TestAdvCancelMidBatchLeaksNoGoroutines cancels a turn while a wide batch
// is in flight and checks the four things that must hold: no call queued
// behind the full pool passes the post-abort admission gate, every refused
// call gets a canceled result, started calls balance tool.start/tool.end,
// and no goroutine remains once runToolCalls returns.
func TestAdvCancelMidBatchLeaksNoGoroutines(t *testing.T) {
	const n = 20

	runtime.GC()
	before := runtime.NumGoroutine()

	var mu sync.Mutex
	starts, ends := map[string]int{}, map[string]int{}

	// Cancel only once the pool is FULL, so this exercises cancelling a
	// wide in-flight batch rather than just post-cancel admission refusal.
	entered := make(chan struct{}, n)
	ctx, cancel := context.WithCancel(context.Background())

	s := NewSession(Config{
		ToolConcurrency: 8,
		OnEvent: func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.Type {
			case EventToolStart:
				starts[ev.ToolCall.CallID]++
			case EventToolEnd:
				ends[ev.ToolCall.CallID]++
			}
		},
	})
	s.tools["blocker"] = batchTool("blocker", func(ctx context.Context, _ string) {
		entered <- struct{}{}
		<-ctx.Done() // honor cancellation
	})

	calls := make([]*message.ToolCall, n)
	for i := range calls {
		calls[i] = batchCall("blocker", fmt.Sprintf("c%d", i))
	}

	go func() {
		for i := 0; i < 8; i++ { // the cap: every slot occupied
			<-entered
		}
		cancel()
	}()
	results := s.runToolCalls(ctx, asstWith(calls...))
	cancel()

	wantOrder(t, results, calls...)

	mu.Lock()
	for id, nStart := range starts {
		if ends[id] != nStart {
			t.Errorf("call %s: %d tool.start events but %d tool.end — event stream UNBALANCED", id, nStart, ends[id])
		}
	}
	nStarted := len(starts)
	started := make(map[string]bool, nStarted)
	for id := range starts {
		started[id] = true
	}
	mu.Unlock()
	t.Logf("cancelled batch of %d with the pool full: %d calls started (the rest were refused admission), all results present, events balanced", n, nStarted)
	if nStarted != 8 {
		t.Errorf("%d calls started, want exactly the cap (8) — no call queued behind the full pool may pass the post-abort admission gate", nStarted)
	}
	canceled := 0
	for i, p := range results {
		tr := p.(*message.ToolResult)
		if started[calls[i].CallID] {
			continue
		}
		canceled++
		if !tr.IsError || partsText(tr.Content) != toolCallCanceledText {
			t.Errorf("unstarted call %s result = error:%v %q, want the synthetic canceled result %q",
				calls[i].CallID, tr.IsError, partsText(tr.Content), toolCallCanceledText)
		}
	}
	if canceled != n-8 {
		t.Errorf("%d calls carried canceled results, want %d", canceled, n-8)
	}

	// Goroutines unwind asynchronously; give them a bounded chance to.
	var after int
	for i := 0; i < 100; i++ {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after > before+2 {
		t.Errorf("goroutines: %d before, %d after a cancelled %d-call batch — leak", before, after, n)
	}
}

// TestAdvEveryToolStartHasAnEndUnderPanicAndCancel hammers the balanced-
// events guarantee with a batch that mixes panicking, cancelled and normal
// calls.
func TestAdvEveryToolStartHasAnEndUnderPanicAndCancel(t *testing.T) {
	var mu sync.Mutex
	starts, ends := map[string]int{}, map[string]int{}

	s := NewSession(Config{
		ToolConcurrency: 8,
		OnEvent: func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.Type {
			case EventToolStart:
				starts[ev.ToolCall.CallID]++
			case EventToolEnd:
				ends[ev.ToolCall.CallID]++
			}
		},
	})
	s.tools["boom"] = batchTool("boom", func(context.Context, string) { panic("adversarial panic") })
	s.tools["fine"] = batchTool("fine", nil)

	var calls []*message.ToolCall
	for i := 0; i < 12; i++ {
		name := "fine"
		if i%3 == 0 {
			name = "boom"
		}
		calls = append(calls, batchCall(name, fmt.Sprintf("c%d", i)))
	}

	results := s.runToolCalls(context.Background(), asstWith(calls...))
	wantOrder(t, results, calls...)

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != len(calls) {
		t.Errorf("%d distinct tool.start events, want %d", len(starts), len(calls))
	}
	for id, nStart := range starts {
		if nStart != 1 {
			t.Errorf("call %s emitted %d tool.start events, want 1", id, nStart)
		}
		if ends[id] != 1 {
			t.Errorf("call %s emitted %d tool.end events, want exactly 1 (a panicking tool must still close its pair)", id, ends[id])
		}
	}
	panics := 0
	for i, p := range results {
		if tr := p.(*message.ToolResult); tr.IsError && strings.Contains(partsText(tr.Content), "panicked") {
			panics++
			_ = i
		}
	}
	if panics != 4 {
		t.Errorf("%d panic results, want 4 (one per panicking call, siblings unaffected)", panics)
	}
}

// ---- Finding 1: retention accounting under a wide batch ----

// TestAdvRetentionCeilingExactUnderWideBatch stresses the per-session
// retained-bytes ceiling with far more concurrent retentions than the
// ceiling admits. The ceiling's check-then-act is split across two
// separate s.mu sections, so it is only safe because retention runs at the
// JOIN, single-threaded, in call order. If it ever moves back inside a
// worker, this test should overshoot.
func TestAdvRetentionCeilingExactUnderWideBatch(t *testing.T) {
	const n = 32
	const each = 8000
	const ceiling = 5 * each

	dir := t.TempDir()
	s := NewSession(Config{
		SessionDir:              dir,
		ToolResultInlineBytes:   500,
		ToolResultRetainedBytes: ceiling,
		ToolConcurrency:         8,
	})
	s.tools["big"] = Tool{
		Def: provider.ToolDef{Name: "big", Description: "d", InputSchema: json.RawMessage(`{}`)},
		Run: func(_ context.Context, _ *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Call string `json:"call"`
			}
			_ = json.Unmarshal(args, &in)
			return message.Parts{&message.Text{Text: in.Call + strings.Repeat("q", each)}}, nil
		},
	}

	calls := make([]*message.ToolCall, n)
	for i := range calls {
		calls[i] = batchCall("big", fmt.Sprintf("c%02d", i))
	}
	results := s.runToolCalls(context.Background(), asstWith(calls...))
	wantOrder(t, results, calls...)

	s.mu.Lock()
	used, handles := s.toolResultBytes, len(s.toolResults)
	s.mu.Unlock()

	// On-disk truth, not just the counter.
	var onDisk int64
	entries, _ := os.ReadDir(s.toolResultsDir())
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			onDisk += fi.Size()
		}
	}
	t.Logf("%d concurrent oversized results, ceiling %d: counter=%d on-disk=%d handles=%d",
		n, ceiling, used, onDisk, handles)
	if used > ceiling {
		t.Errorf("counter %d OVER the %d ceiling — concurrent check-then-act overshot", used, ceiling)
	}
	if onDisk > int64(ceiling) {
		t.Errorf("on-disk %d OVER the %d ceiling", onDisk, ceiling)
	}
	if handles == 0 || handles >= n {
		t.Errorf("handles=%d of %d; want some but not all, so the ceiling actually bound", handles, n)
	}

	// Handles must be minted in CALL order, not completion order.
	var seen []string
	for _, p := range results {
		txt := partsText(p.(*message.ToolResult).Content)
		if i := strings.Index(txt, "handle=trh_"); i >= 0 {
			rest := txt[i+len("handle=trh_"):]
			end := strings.IndexAny(rest, " \n")
			if end < 0 {
				end = len(rest)
			}
			seen = append(seen, rest[:end])
		}
	}
	for i := 1; i < len(seen); i++ {
		a, b := seen[i-1], seen[i]
		if len(a) > len(b) || (len(a) == len(b) && a >= b) {
			t.Errorf("handles not minted in call order: %v", seen)
			break
		}
	}
	t.Logf("handles in call order: trh_%s", strings.Join(seen, ", trh_"))
}

// ---- Finding 5: hook ordering ----

// TestAdvHookPhasesOrderedPerCallAcrossABatch pins what the hook contract
// does and does not promise under a parallel batch. WITHIN one call the
// phases stay strictly ordered (before, then the tool, then after) and each
// fires exactly once. ACROSS calls the relative order is completion order,
// which is the documented, accepted change.
func TestAdvHookPhasesOrderedPerCallAcrossABatch(t *testing.T) {
	const n = 8
	var mu sync.Mutex
	phases := map[string][]string{}

	record := func(call, phase string) {
		mu.Lock()
		phases[call] = append(phases[call], phase)
		mu.Unlock()
	}

	hooks := &phaseHooks{
		onBefore: func(req *plugin.ToolExecuteBeforeRequest) { record(req.CallID, "before") },
		onAfter:  func(req *plugin.ToolExecuteAfterRequest) { record(req.CallID, "after") },
	}

	s := NewSession(Config{ToolConcurrency: 8, Hooks: hooks})
	s.tools["t"] = batchTool("t", func(_ context.Context, call string) {
		record("tc_"+call, "tool")
	})

	calls := make([]*message.ToolCall, n)
	for i := range calls {
		calls[i] = batchCall("t", fmt.Sprintf("c%d", i))
	}
	results := s.runToolCalls(context.Background(), asstWith(calls...))
	wantOrder(t, results, calls...)

	mu.Lock()
	defer mu.Unlock()
	if len(phases) != n {
		t.Fatalf("saw %d calls in hook records, want %d: %v", len(phases), n, phases)
	}
	for call, got := range phases {
		want := []string{"before", "tool", "after"}
		if len(got) != len(want) {
			t.Errorf("call %s phases = %v, want exactly %v (each hook once)", call, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("call %s phases = %v, want %v", call, got, want)
				break
			}
		}
	}
}

// ---- Finding 6: does the exclusion guard fail safe or fail open? ----

// TestAdvUnparseableKeyArgsFallBackToOneSharedKey probes the guard's
// failure mode. A file tool whose args do not carry a usable path cannot
// have its path computed, and the question is whether that yields NO key
// (fail open, calls race) or a SHARED key (fail safe, calls serialize).
func TestAdvUnparseableKeyArgsFallBackToOneSharedKey(t *testing.T) {
	s := NewSession(Config{WorkDir: t.TempDir()})
	for _, args := range []string{`{}`, `{"path":""}`, `not json at all`, `{"path":123}`} {
		got := filePathKey(s, json.RawMessage(args))
		if got == "" {
			t.Errorf("args %s produced an EMPTY key — calls would run unserialized (fail OPEN)", args)
			continue
		}
		if got != filePathKeyPrefix+"<unparsed>" {
			t.Logf("args %s -> %q", args, got)
		}
	}
	// All unparseable spellings must land on ONE key so they serialize
	// with each other, not on distinct keys.
	a := filePathKey(s, json.RawMessage(`{}`))
	b := filePathKey(s, json.RawMessage(`nonsense`))
	if a != b {
		t.Errorf("two unparseable arg shapes took different keys (%q, %q) — they would race", a, b)
	}
}

// TestAdvSequentialFloorCannotBeEscaped checks the cap resolution itself
// never fails open into unbounded parallelism.
func TestAdvSequentialFloorCannotBeEscaped(t *testing.T) {
	for _, in := range []int{-1, -8, -1 << 30} {
		if got := resolveToolConcurrency(in); got != 1 {
			t.Errorf("resolveToolConcurrency(%d) = %d, want 1 (a negative value must clamp to sequential, never unbounded)", in, got)
		}
	}
	if got := resolveToolConcurrency(0); got != defaultToolConcurrency {
		t.Errorf("resolveToolConcurrency(0) = %d, want the package default %d", got, defaultToolConcurrency)
	}
}

// ---- Finding 7: is the synchronization guarding the right thing? ----

// TestAdvSharedSessionStateUnderWideMixedBatch drives every piece of
// session state a batch touches at once — events, the exec counter,
// retention, the read-hash set, per-path keys — with the race detector as
// the oracle. It is deliberately mixed: same-path file tools beside
// unrelated ones, so both the keyed and unkeyed paths run together.
func TestAdvSharedSessionStateUnderWideMixedBatch(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(shared, []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}

	var events int64
	s := NewSession(Config{
		WorkDir:               dir,
		SessionDir:            dir,
		ToolResultInlineBytes: 400,
		ToolConcurrency:       8,
		OnEvent:               func(Event) { atomic.AddInt64(&events, 1) },
	})
	s.tools["chatty"] = Tool{
		Def: provider.ToolDef{Name: "chatty", Description: "d", InputSchema: json.RawMessage(`{}`)},
		Run: func(_ context.Context, sess *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Call string `json:"call"`
			}
			_ = json.Unmarshal(args, &in)
			_ = sess.Usage()
			return message.Parts{&message.Text{Text: in.Call + strings.Repeat("m", 2000)}}, nil
		},
	}

	var calls []*message.ToolCall
	for i := 0; i < 6; i++ {
		calls = append(calls, batchCall("chatty", fmt.Sprintf("c%d", i)))
		calls = append(calls, &message.ToolCall{
			CallID:    fmt.Sprintf("rd%d", i),
			Name:      "read_file",
			Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, shared)),
		})
		calls = append(calls, &message.ToolCall{
			CallID:    fmt.Sprintf("wr%d", i),
			Name:      "write_file",
			Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"round-%d"}`, shared, i)),
		})
		calls = append(calls, &message.ToolCall{
			CallID:    fmt.Sprintf("ls%d", i),
			Name:      "ls",
			Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, dir)),
		})
	}

	results := s.runToolCalls(context.Background(), asstWith(calls...))
	wantOrder(t, results, calls...)

	// The same-path read/write chain ran in call order, so the file ends
	// on the LAST write.
	if got, _ := os.ReadFile(shared); string(got) != "round-5" {
		t.Errorf("shared file = %q, want round-5 (the last same-path write in call order)", got)
	}
	s.mu.Lock()
	execs := s.toolExecCount
	s.mu.Unlock()
	if execs != len(calls) {
		t.Errorf("toolExecCount = %d, want %d (one per call, no lost increment)", execs, len(calls))
	}
	if n := atomic.LoadInt64(&events); n < int64(2*len(calls)) {
		t.Errorf("emitted %d events, want at least %d (a start and an end per call)", n, 2*len(calls))
	}
}

// TestAdvKeyChainIsOnlyUsedFromTheSubmittingGoroutine guards keyChain's
// documented no-lock premise: it carries no mutex because wait() is called
// only from runParallelSegment's setup loop, never from a worker. A future
// change that moves the call into a goroutine must add a lock with it, and
// this test is the tripwire.
func TestAdvKeyChainIsOnlyUsedFromTheSubmittingGoroutine(t *testing.T) {
	src, err := os.ReadFile("toolexec.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (s *Session) runParallelSegment")
	if i < 0 {
		t.Fatal("runParallelSegment not found")
	}
	fn := body[i:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}
	callIdx := strings.Index(fn, "chain.wait(")
	goIdx := strings.Index(fn, "go func(j job)")
	if callIdx < 0 || goIdx < 0 {
		t.Fatalf("expected both chain.wait( and the worker goroutine in runParallelSegment")
	}
	if callIdx > goIdx {
		t.Errorf("chain.wait is called at/after the worker goroutine launch — keyChain has no mutex and now needs one")
	}
}

// phaseHooks records the tool.execute.before/after phases per call. Every
// method must be safe for concurrent use: the engine dispatches hooks from
// several goroutines at once for one batch.
type phaseHooks struct {
	onBefore func(*plugin.ToolExecuteBeforeRequest)
	onAfter  func(*plugin.ToolExecuteAfterRequest)
}

func (h *phaseHooks) ChatParams(_ context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams {
	return req.Params
}
func (h *phaseHooks) ChatMessage(_ context.Context, req *plugin.ChatMessageRequest) message.Message {
	return req.Message
}
func (h *phaseHooks) SystemTransform(context.Context, *plugin.SystemTransformRequest) []string {
	return nil
}
func (h *phaseHooks) ShellEnv(context.Context, *plugin.ShellEnvRequest) map[string]string {
	return nil
}
func (h *phaseHooks) ToolExecuteBefore(_ context.Context, req *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string) {
	h.onBefore(req)
	return req.Args, ""
}
func (h *phaseHooks) ToolExecuteAfter(_ context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts {
	h.onAfter(req)
	return req.Output
}
func (h *phaseHooks) ExecuteTool(_ context.Context, req *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error) {
	return nil, fmt.Errorf("plugin: no plugin provides tool %q", req.Tool)
}
func (h *phaseHooks) Emit([]plugin.Event)     {}
func (h *phaseHooks) Plugins() []plugin.Info  { return nil }
func (h *phaseHooks) Tools() []plugin.ToolDef { return nil }
