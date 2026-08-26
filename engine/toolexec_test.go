package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// Tests for the concurrent tool-call batch executor (toolexec.go).
//
// Concurrency is proved by RENDEZVOUS, never by timing: a fake tool blocks
// until a sibling has entered, so an executor that runs one call at a time
// cannot make progress. Those tests run inside a testing/synctest bubble,
// where "cannot make progress" is reported at once as a deadlock instead of
// hanging on the wall clock. Wall-clock claims are measured with the
// bubble's FAKE clock, where a time.Sleep costs nothing and elapsed time is
// exact.

// batchTool builds a fake built-in tool named name whose Run calls fn. The
// tool echoes name back as its output, so a result can be traced to the
// call that produced it.
func batchTool(name string, fn func(ctx context.Context, call string)) Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        name,
			Description: "test tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Run: func(ctx context.Context, _ *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Call string `json:"call"`
			}
			_ = json.Unmarshal(args, &in)
			if fn != nil {
				fn(ctx, in.Call)
			}
			return message.Parts{&message.Text{Text: in.Call}}, nil
		},
	}
}

// batchCall builds one ToolCall for tool name, tagged with call so the
// fake tool and the assertions can identify it.
func batchCall(name, call string) *message.ToolCall {
	return &message.ToolCall{
		CallID:    "tc_" + call,
		Name:      name,
		Arguments: json.RawMessage(fmt.Sprintf(`{"call":%q}`, call)),
	}
}

func asstWith(calls ...*message.ToolCall) *message.Message {
	parts := make(message.Parts, len(calls))
	for i, c := range calls {
		parts[i] = c
	}
	return &message.Message{ID: "msg_a", Role: message.RoleAssistant, Parts: parts}
}

// resultOrder returns each ToolResult's CallID, in the order the executor
// returned them. It also fails the test if any part is not a ToolResult.
func resultOrder(t *testing.T, parts message.Parts) []string {
	t.Helper()
	ids := make([]string, len(parts))
	for i, p := range parts {
		tr, ok := p.(*message.ToolResult)
		if !ok {
			t.Fatalf("results[%d] = %T, want *message.ToolResult", i, p)
		}
		ids[i] = tr.CallID
	}
	return ids
}

// wantOrder asserts the results pair one-to-one with calls, in call order.
// It checks BOTH directions: no call is missing a result, and no result is
// a duplicate or a surplus.
func wantOrder(t *testing.T, parts message.Parts, calls ...*message.ToolCall) {
	t.Helper()
	if len(parts) != len(calls) {
		t.Fatalf("results = %d, want exactly %d (one per tool call)", len(parts), len(calls))
	}
	got := resultOrder(t, parts)
	seen := make(map[string]int, len(got))
	for i, c := range calls {
		if got[i] != c.CallID {
			t.Errorf("results[%d] call id = %q, want %q (results must join in CALL order)", i, got[i], c.CallID)
		}
		seen[got[i]]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("call id %q appears %d times in the results, want exactly 1", id, n)
		}
	}
}

// TestBatchWallClockIsMaxNotSum proves the batch runs concurrently: six
// tools that each take three seconds finish the batch in three seconds,
// not eighteen.
//
// The measurement runs inside a synctest bubble, so time is FAKE and the
// elapsed value is exact rather than approximate — a sanctioned time
// mechanism, unlike a real sleep (see AGENTS.md, Testing).
func TestBatchWallClockIsMaxNotSum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n = 6
		const each = 3 * time.Second

		s := NewSession(Config{})
		s.tools["slow"] = batchTool("slow", func(context.Context, string) {
			time.Sleep(each)
		})

		calls := make([]*message.ToolCall, n)
		for i := range calls {
			calls[i] = batchCall("slow", fmt.Sprintf("c%d", i))
		}

		start := time.Now()
		results := s.runToolCalls(context.Background(), asstWith(calls...))
		elapsed := time.Since(start)

		wantOrder(t, results, calls...)
		if elapsed != each {
			t.Fatalf("batch of %d took %v, want %v (the longest call, not the sum %v)",
				n, elapsed, each, n*each)
		}
	})
}

// TestBatchResultsJoinInCallOrderUnderReversedCompletion proves the join is
// order-stable. The tools are released in REVERSE call order, so completion
// order is exactly the opposite of call order, and the results must still
// come back in call order.
//
// Reading every entry of entered before releasing anything also proves all
// five calls were in flight together.
func TestBatchResultsJoinInCallOrderUnderReversedCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n = 5

		entered := make(chan string, n)
		release := make([]chan struct{}, n)
		for i := range release {
			release[i] = make(chan struct{})
		}
		index := map[string]int{}
		for i := range n {
			index[fmt.Sprintf("c%d", i)] = i
		}

		s := NewSession(Config{})
		s.tools["held"] = batchTool("held", func(_ context.Context, call string) {
			entered <- call
			<-release[index[call]]
		})

		calls := make([]*message.ToolCall, n)
		for i := range calls {
			calls[i] = batchCall("held", fmt.Sprintf("c%d", i))
		}

		var results message.Parts
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
		}()

		for range n {
			<-entered
		}
		var completion []string
		for i := n - 1; i >= 0; i-- {
			close(release[i])
			completion = append(completion, fmt.Sprintf("c%d", i))
			synctest.Wait()
		}
		synctest.Wait()

		if completion[0] != "c4" {
			t.Fatalf("completion order = %v, want it reversed", completion)
		}
		wantOrder(t, results, calls...)
	})
}

// TestBatchSequentialModeNeverOverlaps proves Config.ToolConcurrency 1
// restores the pre-parallel path: no two calls are ever in flight at once,
// and they run in call order.
//
// This test deliberately uses NO rendezvous — a rendezvous is exactly what
// a sequential executor cannot satisfy. It observes the in-flight counter
// instead.
func TestBatchSequentialModeNeverOverlaps(t *testing.T) {
	const n = 5

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	var order []string

	s := NewSession(Config{ToolConcurrency: 1})
	s.tools["counted"] = batchTool("counted", func(_ context.Context, call string) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		order = append(order, call)
		mu.Unlock()
		mu.Lock()
		inFlight--
		mu.Unlock()
	})

	calls := make([]*message.ToolCall, n)
	for i := range calls {
		calls[i] = batchCall("counted", fmt.Sprintf("c%d", i))
	}
	results := s.runToolCalls(context.Background(), asstWith(calls...))

	wantOrder(t, results, calls...)
	if maxInFlight != 1 {
		t.Errorf("max calls in flight = %d, want 1 in sequential mode", maxInFlight)
	}
	for i, call := range order {
		if want := fmt.Sprintf("c%d", i); call != want {
			t.Errorf("execution order[%d] = %q, want %q", i, call, want)
		}
	}
}

// TestBatchConcurrencyCapIsEnforced proves the cap both BOUNDS the batch
// and is REACHED. Nine calls run with a cap of three, and every call
// blocks inside the tool. synctest.Wait then returns only once every
// goroutine in the bubble is durably blocked, so the number of calls
// inside the tool at that moment is the true in-flight maximum: exactly
// three. An executor that ignored the cap would have all nine inside.
//
// Counting a running maximum instead would NOT prove this. A barrier that
// releases each wave lets an unbounded pool look bounded, because the
// early calls can exit before the later ones enter.
func TestBatchConcurrencyCapIsEnforced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n, capacity = 9, 3

		entered := make(chan string, n)
		releaseAll := make(chan struct{})

		s := NewSession(Config{ToolConcurrency: capacity})
		s.tools["capped"] = batchTool("capped", func(_ context.Context, call string) {
			entered <- call
			<-releaseAll
		})

		calls := make([]*message.ToolCall, n)
		for i := range calls {
			calls[i] = batchCall("capped", fmt.Sprintf("c%d", i))
		}
		var results message.Parts
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
		}()

		synctest.Wait()
		if got := len(entered); got != capacity {
			t.Fatalf("%d calls were inside the tool at once, want exactly %d (the cap)", got, capacity)
		}

		close(releaseAll)
		synctest.Wait()

		wantOrder(t, results, calls...)
		if got := len(entered); got != n {
			t.Errorf("%d calls ran in total, want %d", got, n)
		}
	})
}

// TestBatchSerialToolIsABarrierOnBothSides proves a Serial tool splits the
// batch. The batch is [p0, p1, barrier, p2, p3]. p0 and p1 rendezvous with
// each other, and p2 with p3, so each parallel run must genuinely overlap
// or the bubble deadlocks. The recorded log must then show both leading
// calls finished before the barrier started, and the barrier finished
// before either trailing call started.
func TestBatchSerialToolIsABarrierOnBothSides(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var log []string
		record := func(s string) {
			mu.Lock()
			log = append(log, s)
			mu.Unlock()
		}

		lead := make(chan struct{}, 2)
		leadGo := make(chan struct{})
		trail := make(chan struct{}, 2)
		trailGo := make(chan struct{})

		s := NewSession(Config{ToolConcurrency: 8})
		s.tools["par"] = batchTool("par", func(_ context.Context, call string) {
			record("enter " + call)
			switch call {
			case "p0", "p1":
				lead <- struct{}{}
				<-leadGo
			default:
				trail <- struct{}{}
				<-trailGo
			}
			record("exit " + call)
		})
		barrier := batchTool("barrier", func(context.Context, string) {
			record("enter b")
			record("exit b")
		})
		barrier.Serial = true
		s.tools["barrier"] = barrier

		calls := []*message.ToolCall{
			batchCall("par", "p0"), batchCall("par", "p1"),
			batchCall("barrier", "b"),
			batchCall("par", "p2"), batchCall("par", "p3"),
		}

		var results message.Parts
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
		}()

		// Both leading calls must be inside before either may finish.
		<-lead
		<-lead
		close(leadGo)
		// Both trailing calls must be inside before either may finish.
		<-trail
		<-trail
		close(trailGo)
		synctest.Wait()

		wantOrder(t, results, calls...)

		mu.Lock()
		defer mu.Unlock()
		pos := func(entry string) int {
			for i, e := range log {
				if e == entry {
					return i
				}
			}
			t.Fatalf("log has no %q entry: %v", entry, log)
			return -1
		}
		for _, before := range []string{"exit p0", "exit p1"} {
			if pos(before) > pos("enter b") {
				t.Errorf("%q happened after the barrier started: %v", before, log)
			}
		}
		for _, after := range []string{"enter p2", "enter p3"} {
			if pos(after) < pos("exit b") {
				t.Errorf("%q happened before the barrier finished: %v", after, log)
			}
		}
	})
}

// TestBatchSameKeyCallsSerializeInCallOrder proves per-key exclusion. Three
// calls share one path key and one call uses a different path. The
// same-key calls must never overlap and must run in call order; the
// different-key call must overlap with the first same-key call, which the
// rendezvous forces (a wholly serialized executor deadlocks the bubble).
func TestBatchSameKeyCallsSerializeInCallOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		inFlightSameKey := 0
		var keyOrder []string
		overlap := make(chan struct{})
		otherIn := make(chan struct{})

		keyed := func(name string) Tool {
			tool := batchTool(name, nil)
			tool.Key = filePathKey
			return tool
		}

		s := NewSession(Config{WorkDir: "/w", ToolConcurrency: 8})
		same := keyed("same")
		same.Run = func(_ context.Context, _ *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Call string `json:"call"`
			}
			_ = json.Unmarshal(args, &in)
			mu.Lock()
			inFlightSameKey++
			if inFlightSameKey > 1 {
				t.Errorf("two same-key calls ran at once (%q)", in.Call)
			}
			keyOrder = append(keyOrder, in.Call)
			first := len(keyOrder) == 1
			mu.Unlock()
			if first {
				// The first same-key call waits for the OTHER-key call,
				// so the two keys must genuinely run side by side.
				<-otherIn
			}
			mu.Lock()
			inFlightSameKey--
			mu.Unlock()
			return message.Parts{&message.Text{Text: in.Call}}, nil
		}
		s.tools["same"] = same
		other := keyed("other")
		other.Run = func(context.Context, *Session, json.RawMessage) (message.Parts, error) {
			close(otherIn)
			<-overlap
			return message.Parts{&message.Text{Text: "other"}}, nil
		}
		s.tools["other"] = other

		mkCall := func(name, call, path string) *message.ToolCall {
			return &message.ToolCall{
				CallID:    "tc_" + call,
				Name:      name,
				Arguments: json.RawMessage(fmt.Sprintf(`{"call":%q,"path":%q}`, call, path)),
			}
		}
		calls := []*message.ToolCall{
			mkCall("same", "s0", "shared.txt"),
			mkCall("same", "s1", "shared.txt"),
			mkCall("other", "o0", "unrelated.txt"),
			mkCall("same", "s2", "shared.txt"),
		}

		var results message.Parts
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
		}()

		<-otherIn
		close(overlap)
		synctest.Wait()

		wantOrder(t, results, calls...)
		mu.Lock()
		defer mu.Unlock()
		want := []string{"s0", "s1", "s2"}
		if len(keyOrder) != len(want) {
			t.Fatalf("same-key execution order = %v, want %v", keyOrder, want)
		}
		for i, c := range want {
			if keyOrder[i] != c {
				t.Errorf("same-key execution order = %v, want %v (call order)", keyOrder, want)
				break
			}
		}
	})
}

// TestFileToolsShareOnePathKeyNamespace is the production wiring check for
// the key itself: read_file, write_file and edit_file must resolve ONE key
// per file, so a read racing a write or an edit to that file cannot happen.
// It also pins the two normalization rules the key promises.
func TestFileToolsShareOnePathKeyNamespace(t *testing.T) {
	s := NewSession(Config{WorkDir: "/work"})
	args := func(path string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))
	}

	base := s.toolKey("edit_file", args("a.txt"))
	if base == "" {
		t.Fatal("edit_file has no resource key: same-path edits would run concurrently")
	}
	for _, tool := range []string{"read_file", "write_file"} {
		if got := s.toolKey(tool, args("a.txt")); got != base {
			t.Errorf("%s key = %q, want %q: all three file tools must share one namespace", tool, got, base)
		}
	}
	if got := s.toolKey("edit_file", args("b.txt")); got == base {
		t.Error("two different paths share a key: different files must run concurrently")
	}
	// Normalization: a dot-dot alias and an absolute spelling of the same
	// file must key the same.
	if got := s.toolKey("edit_file", args("sub/../a.txt")); got != base {
		t.Errorf("key for %q = %q, want %q: a dot-dot alias must not bypass the key", "sub/../a.txt", got, base)
	}
	if got := s.toolKey("edit_file", args("/work/a.txt")); got != base {
		t.Errorf("absolute key = %q, want %q", got, base)
	}
	// An unparseable call still gets a key, so it cannot slip past
	// exclusion entirely.
	if got := s.toolKey("edit_file", json.RawMessage(`{`)); got == "" {
		t.Error("an unparseable file call has no key: it would run unserialized")
	}
}

// TestEditFileSamePathBatchAppliesInCallOrder drives the REAL file tools
// through the production entry point. Two edits chained on one file
// (a->b, then b->c) can only both succeed if they run in call order
// against each other's output.
func TestEditFileSamePathBatchAppliesInCallOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewSession(Config{WorkDir: dir})
	calls := []*message.ToolCall{
		{CallID: "tc1", Name: "edit_file", Arguments: json.RawMessage(`{"path":"f.txt","old_string":"a","new_string":"b"}`)},
		{CallID: "tc2", Name: "edit_file", Arguments: json.RawMessage(`{"path":"f.txt","old_string":"b","new_string":"c"}`)},
	}
	results := s.runToolCalls(context.Background(), asstWith(calls...))
	wantOrder(t, results, calls...)
	for i, p := range results {
		if tr := p.(*message.ToolResult); tr.IsError {
			t.Errorf("edit %d failed: %s", i, tr.Content.Text())
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "c" {
		t.Errorf("file = %q, want %q: the two same-path edits did not apply in call order", got, "c")
	}
}

// TestBatchPartialFailureLetsSiblingsFinish proves one failing call never
// cancels its siblings: every call still returns its own result, and the
// error lands on the right call id. The rendezvous forces the siblings to
// be in flight while the failing call is inside, so this is a real
// concurrent partial failure, not a sequential one.
func TestBatchPartialFailureLetsSiblingsFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan string, 3)
		release := make(chan struct{})

		s := NewSession(Config{ToolConcurrency: 8})
		s.tools["ok"] = batchTool("ok", func(_ context.Context, call string) {
			entered <- call
			<-release
		})
		s.tools["boom"] = Tool{
			Def: provider.ToolDef{Name: "boom", Description: "fails", InputSchema: json.RawMessage(`{}`)},
			Run: func(context.Context, *Session, json.RawMessage) (message.Parts, error) {
				entered <- "boom"
				<-release
				return nil, fmt.Errorf("deliberate failure")
			},
		}

		calls := []*message.ToolCall{
			batchCall("ok", "c0"),
			{CallID: "tc_boom", Name: "boom", Arguments: json.RawMessage(`{}`)},
			batchCall("ok", "c2"),
		}

		var results message.Parts
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
		}()
		for range 3 {
			<-entered
		}
		close(release)
		synctest.Wait()

		wantOrder(t, results, calls...)
		for i, p := range results {
			tr := p.(*message.ToolResult)
			wantErr := tr.CallID == "tc_boom"
			if tr.IsError != wantErr {
				t.Errorf("results[%d] (%s) IsError = %v, want %v", i, tr.CallID, tr.IsError, wantErr)
			}
		}
	})
}

// TestBatchCancellationStillYieldsOneResultPerCall is the orphan-pairing
// guard (AGENTS.md, NEP-5272): a tool_use block with no tool_result wedges
// a session forever, so an aborted turn must still produce exactly one
// result per call — never fewer, and never a duplicate.
//
// Two shapes. In "already canceled" no call runs at all. In "canceled in
// flight" every call is inside the tool when the abort lands, and the
// calls queued behind the cap are admitted after it.
func TestBatchCancellationStillYieldsOneResultPerCall(t *testing.T) {
	const n = 6

	t.Run("already canceled", func(t *testing.T) {
		var ran int
		var mu sync.Mutex
		s := NewSession(Config{ToolConcurrency: 2})
		s.tools["never"] = batchTool("never", func(context.Context, string) {
			mu.Lock()
			ran++
			mu.Unlock()
		})
		calls := make([]*message.ToolCall, n)
		for i := range calls {
			calls[i] = batchCall("never", fmt.Sprintf("c%d", i))
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		results := s.runToolCalls(ctx, asstWith(calls...))

		wantOrder(t, results, calls...)
		for i, p := range results {
			tr := p.(*message.ToolResult)
			if !tr.IsError {
				t.Errorf("results[%d] is not an error result; a canceled call must say so", i)
			}
			if got := tr.Content.Text(); got != toolCallCanceledText {
				t.Errorf("results[%d] = %q, want %q", i, got, toolCallCanceledText)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if ran != 0 {
			t.Errorf("%d calls ran after the turn was canceled, want 0", ran)
		}
	})

	t.Run("canceled in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			entered := make(chan struct{}, 2)

			s := NewSession(Config{ToolConcurrency: 2})
			s.tools["waits"] = batchTool("waits", func(ctx context.Context, _ string) {
				entered <- struct{}{}
				<-ctx.Done()
			})
			calls := make([]*message.ToolCall, n)
			for i := range calls {
				calls[i] = batchCall("waits", fmt.Sprintf("c%d", i))
			}

			var results message.Parts
			go func() {
				results = s.runToolCalls(ctx, asstWith(calls...))
			}()
			// Both calls the cap admits are inside before the abort.
			<-entered
			<-entered
			cancel()
			synctest.Wait()

			wantOrder(t, results, calls...)
		})
	})
}

// TestBatchJournalCommitsInCallOrderWhileEventsInterleave drives a REAL
// turn through Session.Prompt and asserts the two halves of the approved
// event contract at once. EventToolEnd MAY arrive out of call order —
// events are keyed by call id, so interleaving is expected and correct —
// while the RoleTool message that lands in history MUST be in call order,
// because tool_result order has to match tool_use order on the wire.
func TestBatchJournalCommitsInCallOrderWhileEventsInterleave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n = 4

		entered := make(chan string, n)
		release := make([]chan struct{}, n)
		for i := range release {
			release[i] = make(chan struct{})
		}
		index := map[string]int{}
		calls := make([]*message.ToolCall, n)
		for i := range n {
			name := fmt.Sprintf("c%d", i)
			index[name] = i
			calls[i] = batchCall("held", name)
		}
		parts := make(message.Parts, n)
		for i, c := range calls {
			parts[i] = c
		}

		prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
			{{Type: provider.EventDone, Message: &message.Message{
				ID: "msg_a", Role: message.RoleAssistant, Parts: parts,
			}, StopReason: provider.StopToolUse}},
			asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
		}}

		var evMu sync.Mutex
		var toolEnds []string
		s := NewSession(Config{
			Providers: provider.Registry{"test": prov},
			Model:     message.ModelRef{Provider: "test", Model: "m1"},
			// OnEvent is called from several goroutines at once now (see
			// Config.OnEvent): this collector must lock, and the lock is
			// part of what the test documents.
			OnEvent: func(ev Event) {
				if ev.Type != EventToolEnd {
					return
				}
				evMu.Lock()
				toolEnds = append(toolEnds, ev.ToolCall.CallID)
				evMu.Unlock()
			},
		})
		s.tools["held"] = batchTool("held", func(_ context.Context, call string) {
			entered <- call
			<-release[index[call]]
		})

		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := s.Prompt(context.Background(), "go"); err != nil {
				t.Errorf("Prompt: %v", err)
			}
		}()

		for range n {
			<-entered
		}
		// Finish in reverse call order.
		for i := n - 1; i >= 0; i-- {
			close(release[i])
			synctest.Wait()
		}
		<-done

		// History: user, assistant(tool calls), tool(results), assistant.
		h := s.History()
		if len(h) != 4 {
			t.Fatalf("history len = %d, want 4: %+v", len(h), h)
		}
		if h[2].Role != message.RoleTool {
			t.Fatalf("history[2] role = %q, want %q", h[2].Role, message.RoleTool)
		}
		wantOrder(t, h[2].Parts, calls...)

		// The events tell the other half of the story: they came back in
		// completion order, which is the REVERSE of call order here.
		evMu.Lock()
		defer evMu.Unlock()
		if len(toolEnds) != n {
			t.Fatalf("EventToolEnd count = %d, want %d", len(toolEnds), n)
		}
		if toolEnds[0] != calls[n-1].CallID {
			t.Fatalf("EventToolEnd order = %v; this test needs completion order to differ from call order", toolEnds)
		}
	})
}

// TestBatchRetentionMintsHandlesInCallOrder proves retention runs at the
// JOIN, in call order, and not inside the workers. Three oversized results
// complete in REVERSE call order; their trh_N handles must still be
// numbered by call position. Concurrent retention would number them by
// completion, which makes a transcript depend on scheduling.
func TestBatchRetentionMintsHandlesInCallOrder(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		const n = 3
		entered := make(chan string, n)
		release := make([]chan struct{}, n)
		for i := range release {
			release[i] = make(chan struct{})
		}
		index := map[string]int{"c0": 0, "c1": 1, "c2": 2}

		s := NewSession(Config{
			SessionDir:              dir,
			ToolResultInlineBytes:   200,
			ToolResultRetainedBytes: 1 << 20,
			ToolConcurrency:         8,
		})
		body := strings.Repeat("x", 4000)
		s.tools["big"] = Tool{
			Def: provider.ToolDef{Name: "big", Description: "d", InputSchema: json.RawMessage(`{}`)},
			Run: func(_ context.Context, _ *Session, args json.RawMessage) (message.Parts, error) {
				var in struct {
					Call string `json:"call"`
				}
				_ = json.Unmarshal(args, &in)
				entered <- in.Call
				<-release[index[in.Call]]
				return message.Parts{&message.Text{Text: in.Call + " " + body}}, nil
			},
		}

		calls := make([]*message.ToolCall, n)
		for i := range calls {
			calls[i] = batchCall("big", fmt.Sprintf("c%d", i))
		}
		var results message.Parts
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
		}()
		for range n {
			<-entered
		}
		for i := n - 1; i >= 0; i-- {
			close(release[i])
			synctest.Wait()
		}
		synctest.Wait()

		wantOrder(t, results, calls...)
		var handles []string
		for i, p := range results {
			text := p.(*message.ToolResult).Content.Text()
			m := toolResultHandleInTextPattern.FindString(text)
			if m == "" {
				t.Fatalf("results[%d] carries no retention handle: %q", i, text)
			}
			handles = append(handles, m)
		}
		want := []string{"trh_1", "trh_2", "trh_3"}
		for i := range want {
			if handles[i] != want[i] {
				t.Fatalf("handles = %v, want %v: retention must mint in CALL order, not completion order", handles, want)
			}
		}
	})
}

// TestBatchRetentionCeilingHoldsAcrossOneBatch is the adversarial-review
// finding on the retained-bytes ceiling. Its check-then-act spans two
// separate s.mu sections, so concurrent retention could let several
// results each observe an uncrossed ceiling and all write. Running
// retention at the join removes the concurrency, so a batch whose results
// individually fit but whose SUM exceeds the cap stops at the cap.
func TestBatchRetentionCeilingHoldsAcrossOneBatch(t *testing.T) {
	dir := t.TempDir()
	const each = 4000
	const ceiling = 2 * each // only two of the four may be retained

	s := NewSession(Config{
		SessionDir:              dir,
		ToolResultInlineBytes:   200,
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
			return message.Parts{&message.Text{Text: in.Call + strings.Repeat("y", each)}}, nil
		},
	}

	calls := make([]*message.ToolCall, 4)
	for i := range calls {
		calls[i] = batchCall("big", fmt.Sprintf("c%d", i))
	}
	results := s.runToolCalls(context.Background(), asstWith(calls...))
	wantOrder(t, results, calls...)

	s.mu.Lock()
	used, handles := s.toolResultBytes, len(s.toolResults)
	s.mu.Unlock()
	if used > ceiling {
		t.Errorf("retained %d bytes, over the %d-byte ceiling", used, ceiling)
	}
	if handles == 0 || handles == len(calls) {
		t.Errorf("retained %d of %d results; want some but not all, so the ceiling actually bound", handles, len(calls))
	}
}

// TestBatchHookRequestsNeverCross is the relayed finding from the plugin
// protocol review. plugin.Host.dispatchChain folds each plugin's response
// into a SHARED *Req as it walks the chain, which is only correct while
// every concurrent tool call owns its OWN request value. Two calls in one
// batch, both passing through a tool.execute.before hook that rewrites
// args, must each get their own rewritten args.
func TestBatchHookRequestsNeverCross(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		arrived := make(chan struct{}, 2)
		proceed := make(chan struct{})

		hooks := &rewritingHooks{
			before: func(req *plugin.ToolExecuteBeforeRequest) json.RawMessage {
				arrived <- struct{}{}
				<-proceed
				return json.RawMessage(fmt.Sprintf(`{"call":%q}`, "rw-"+req.CallID))
			},
		}
		var mu sync.Mutex
		seen := map[string]string{}
		s := NewSession(Config{Hooks: hooks, ToolConcurrency: 8})
		s.tools["echo"] = Tool{
			Def: provider.ToolDef{Name: "echo", Description: "d", InputSchema: json.RawMessage(`{}`)},
			Run: func(_ context.Context, _ *Session, args json.RawMessage) (message.Parts, error) {
				var in struct {
					Call string `json:"call"`
				}
				_ = json.Unmarshal(args, &in)
				mu.Lock()
				seen[in.Call] = in.Call
				mu.Unlock()
				return message.Parts{&message.Text{Text: in.Call}}, nil
			},
		}

		calls := []*message.ToolCall{batchCall("echo", "a"), batchCall("echo", "b")}
		var results message.Parts
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
		}()
		// Both before-hooks must be in flight together.
		<-arrived
		<-arrived
		close(proceed)
		synctest.Wait()

		wantOrder(t, results, calls...)
		mu.Lock()
		defer mu.Unlock()
		for _, c := range calls {
			want := "rw-" + c.CallID
			if _, ok := seen[want]; !ok {
				t.Errorf("call %s did not receive its own rewritten args %q; saw %v", c.CallID, want, seen)
			}
		}
	})
}

// rewritingHooks is a Hooks fake whose tool.execute.before rewrites args
// per call. Every method must be safe for concurrent use: the engine now
// dispatches hooks from several goroutines at once.
type rewritingHooks struct {
	before func(*plugin.ToolExecuteBeforeRequest) json.RawMessage
}

func (h *rewritingHooks) ChatParams(_ context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams {
	return req.Params
}
func (h *rewritingHooks) ChatMessage(_ context.Context, req *plugin.ChatMessageRequest) message.Message {
	return req.Message
}
func (h *rewritingHooks) SystemTransform(context.Context, *plugin.SystemTransformRequest) []string {
	return nil
}
func (h *rewritingHooks) ShellEnv(context.Context, *plugin.ShellEnvRequest) map[string]string {
	return nil
}
func (h *rewritingHooks) ToolExecuteBefore(_ context.Context, req *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string) {
	return h.before(req), ""
}
func (h *rewritingHooks) ToolExecuteAfter(_ context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts {
	return req.Output
}
func (h *rewritingHooks) ExecuteTool(_ context.Context, req *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error) {
	return nil, fmt.Errorf("plugin: no plugin provides tool %q", req.Tool)
}
func (h *rewritingHooks) Emit([]plugin.Event)     {}
func (h *rewritingHooks) Plugins() []plugin.Info  { return nil }
func (h *rewritingHooks) Tools() []plugin.ToolDef { return nil }

// TestBatchTaskSpawnRunsBesideReads is the approved addendum's explicit
// case: batching a task spawn with ordinary reads must work. A task spawn
// is asynchronous and cheap — it hands the child to the SessionManager and
// returns — so it stays in the parallel class rather than being a Serial
// barrier. The batch is [task spawn, read, task spawn]; the two spawns and
// the read must overlap, and the results must still join in call order.
func TestBatchTaskSpawnRunsBesideReads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "r.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two spawned children stream at the same time, so this test needs a
	// provider fake that is safe for concurrent use. scriptedProvider is
	// not: it appends to a slice and bumps a counter with no lock.
	prov := &lockedProvider{name: "test", text: "child done"}
	mgr := NewSessionManager(context.Background(), 0, 0)
	s := mgr.NewRoot(Config{
		Providers:       provider.Registry{"test": prov},
		Model:           message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:         dir,
		ToolConcurrency: 8,
	})

	// The read rendezvouses with both spawns: it does not return until
	// both task calls are inside their own tool. An executor that ran the
	// batch one call at a time could never satisfy that, so this proves
	// real overlap between a task spawn and a read.
	spawnIn := make(chan struct{}, 2)
	readGo := make(chan struct{})
	realTask := s.tools[taskToolName]
	wrapped := realTask
	wrapped.Run = func(ctx context.Context, sess *Session, args json.RawMessage) (message.Parts, error) {
		spawnIn <- struct{}{}
		return realTask.Run(ctx, sess, args)
	}
	s.tools[taskToolName] = wrapped
	realRead := s.tools["read_file"]
	gated := realRead
	gated.Run = func(ctx context.Context, sess *Session, args json.RawMessage) (message.Parts, error) {
		<-readGo
		return realRead.Run(ctx, sess, args)
	}
	s.tools["read_file"] = gated

	spawnArgs := `{"action":"spawn","agent":"general-purpose","prompt":"do a thing"}`
	calls := []*message.ToolCall{
		{CallID: "tc_spawn1", Name: taskToolName, Arguments: json.RawMessage(spawnArgs)},
		{CallID: "tc_read", Name: "read_file", Arguments: json.RawMessage(`{"path":"r.txt"}`)},
		{CallID: "tc_spawn2", Name: taskToolName, Arguments: json.RawMessage(spawnArgs)},
	}

	var results message.Parts
	done := make(chan struct{})
	go func() {
		defer close(done)
		results = s.runToolCalls(context.Background(), asstWith(calls...))
	}()
	<-spawnIn
	<-spawnIn
	close(readGo)
	<-done

	wantOrder(t, results, calls...)
	for i, p := range results {
		if tr := p.(*message.ToolResult); tr.IsError {
			t.Errorf("results[%d] (%s) failed: %s", i, tr.CallID, tr.Content.Text())
		}
	}
	if got := results[1].(*message.ToolResult).Content.Text(); !strings.Contains(got, "hello") {
		t.Errorf("read result = %q, want it to contain the file body", got)
	}
}

// lockedProvider is a concurrency-safe provider fake: every Stream returns
// the same one-message turn. scriptedProvider cannot be used where two
// sessions stream at once — it mutates its own fields with no lock.
type lockedProvider struct {
	name string
	text string

	mu    sync.Mutex
	calls int
}

func (p *lockedProvider) Name() string { return p.name }

func (p *lockedProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &scriptedStream{events: asstTurn(provider.StopEndTurn, &message.Text{Text: p.text})}, nil
}

// TestBatchKeyWaiterDoesNotHoldAPoolSlot is the head-of-line finding from
// the cross-model review. A call waiting for its same-key predecessor must
// not occupy a pool slot while it waits, or an unrelated later call is
// refused admission behind a goroutine that is doing nothing.
//
// The batch is [k1-a, k1-b, free], with a cap of two. k1-a stays inside
// its tool; k1-b can only wait. "free" shares no key with either, so it
// must still be admitted. The test blocks until "free" runs, which an
// executor that admits on the submitting goroutine can never satisfy —
// its two slots are held by k1-a and the waiting k1-b.
func TestBatchKeyWaiterDoesNotHoldAPoolSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		freeRan := make(chan struct{})
		holdA := make(chan struct{})
		aIn := make(chan struct{})

		keyed := func(name, key string) Tool {
			return Tool{
				Def: provider.ToolDef{Name: name, Description: "d", InputSchema: json.RawMessage(`{}`)},
				Key: func(*Session, json.RawMessage) string { return key },
				Run: func(_ context.Context, _ *Session, args json.RawMessage) (message.Parts, error) {
					var in struct {
						Call string `json:"call"`
					}
					_ = json.Unmarshal(args, &in)
					switch in.Call {
					case "a":
						close(aIn)
						<-holdA
					case "free":
						close(freeRan)
					}
					return message.Parts{&message.Text{Text: in.Call}}, nil
				},
			}
		}

		s := NewSession(Config{ToolConcurrency: 2})
		s.tools["k1"] = keyed("k1", "shared")
		s.tools["free"] = keyed("free", "")

		calls := []*message.ToolCall{
			batchCall("k1", "a"), batchCall("k1", "b"), batchCall("free", "free"),
		}
		var results message.Parts
		go func() {
			results = s.runToolCalls(context.Background(), asstWith(calls...))
		}()

		<-aIn
		// The unrelated call must get in while k1-a still holds the key
		// and k1-b is still waiting for it.
		<-freeRan
		close(holdA)
		synctest.Wait()

		wantOrder(t, results, calls...)
	})
}

// TestBatchPanicInToolYieldsOneErrorResult proves the one-result-per-call
// guarantee survives a panicking tool. A panic in a worker goroutine
// cannot be recovered by the join, so without the guard it takes the
// process down and leaves the assistant message's tool_use blocks
// unanswered forever. The panicking call must instead return one error
// result, and its siblings must still return their own.
func TestBatchPanicInToolYieldsOneErrorResult(t *testing.T) {
	for _, tc := range []struct {
		name        string
		concurrency int
	}{
		{"parallel", 8},
		{"sequential", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession(Config{ToolConcurrency: tc.concurrency})
			s.tools["ok"] = batchTool("ok", nil)
			s.tools["panics"] = Tool{
				Def: provider.ToolDef{Name: "panics", Description: "d", InputSchema: json.RawMessage(`{}`)},
				Run: func(context.Context, *Session, json.RawMessage) (message.Parts, error) {
					panic("tool exploded")
				},
			}
			calls := []*message.ToolCall{
				batchCall("ok", "c0"),
				{CallID: "tc_panic", Name: "panics", Arguments: json.RawMessage(`{}`)},
				batchCall("ok", "c2"),
			}
			results := s.runToolCalls(context.Background(), asstWith(calls...))

			wantOrder(t, results, calls...)
			for _, p := range results {
				tr := p.(*message.ToolResult)
				wantErr := tr.CallID == "tc_panic"
				if tr.IsError != wantErr {
					t.Errorf("%s IsError = %v, want %v", tr.CallID, tr.IsError, wantErr)
				}
				if wantErr && !strings.Contains(tr.Content.Text(), toolCallPanicText) {
					t.Errorf("%s = %q, want it to name the panic", tr.CallID, tr.Content.Text())
				}
			}
		})
	}
}

// TestBatchPanickingKeyStillSerializes proves a Key that panics cannot take
// the batch down with no results at all. Key runs on the submitting
// goroutine, before any result slot is filled, so its fallback must be a
// real key — never "", which would silently drop the exclusion the tool
// asked for.
func TestBatchPanickingKeyStillSerializes(t *testing.T) {
	s := NewSession(Config{ToolConcurrency: 8})
	s.tools["bad"] = Tool{
		Def: provider.ToolDef{Name: "bad", Description: "d", InputSchema: json.RawMessage(`{}`)},
		Key: func(*Session, json.RawMessage) string { panic("key exploded") },
		Run: func(context.Context, *Session, json.RawMessage) (message.Parts, error) {
			return message.Parts{&message.Text{Text: "ran"}}, nil
		},
	}
	if got := s.toolKey("bad", json.RawMessage(`{}`)); got == "" {
		t.Fatal("a panicking Key fell back to no key: the tool's exclusion would be dropped")
	}
	calls := []*message.ToolCall{
		{CallID: "tc1", Name: "bad", Arguments: json.RawMessage(`{}`)},
		{CallID: "tc2", Name: "bad", Arguments: json.RawMessage(`{}`)},
	}
	wantOrder(t, s.runToolCalls(context.Background(), asstWith(calls...)), calls...)
}

// TestFilePathKeyIsAbsoluteUnderRelativeWorkDir is the third cross-model
// finding. resolvePath joins a relative argument onto Config.WorkDir, and
// WorkDir itself may be relative, so cleaning alone left one file with two
// keys — and a write could then race an edit on it.
func TestFilePathKeyIsAbsoluteUnderRelativeWorkDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(Config{WorkDir: "."})
	args := func(path string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))
	}
	rel := s.toolKey("edit_file", args("a.txt"))
	abs := s.toolKey("write_file", args(filepath.Join(cwd, "a.txt")))
	if rel != abs {
		t.Errorf("relative key %q != absolute key %q: one file must take one key", rel, abs)
	}
	if !strings.HasPrefix(rel, filePathKeyPrefix+"/") {
		t.Errorf("key %q is not absolute", rel)
	}
}
