package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
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

// TestBatchConcurrencyCapIsEnforced proves the cap both BOUNDS and is
// REACHED. Nine calls run with a cap of three: the barrier releases only
// once three calls are inside at the same time, so a cap that never let
// three run together would deadlock the bubble. The observed maximum must
// then be exactly three — an executor that ignored the cap would admit all
// nine and record nine.
func TestBatchConcurrencyCapIsEnforced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n, capacity = 9, 3

		var mu sync.Mutex
		inFlight, maxInFlight, arrived := 0, 0, 0
		gate := make(chan struct{})

		s := NewSession(Config{ToolConcurrency: capacity})
		s.tools["capped"] = batchTool("capped", func(context.Context, string) {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			arrived++
			mine := gate
			if arrived%capacity == 0 {
				close(gate)
				gate = make(chan struct{})
			}
			mu.Unlock()

			<-mine

			mu.Lock()
			inFlight--
			mu.Unlock()
		})

		calls := make([]*message.ToolCall, n)
		for i := range calls {
			calls[i] = batchCall("capped", fmt.Sprintf("c%d", i))
		}
		results := s.runToolCalls(context.Background(), asstWith(calls...))

		wantOrder(t, results, calls...)
		mu.Lock()
		defer mu.Unlock()
		if maxInFlight != capacity {
			t.Fatalf("max calls in flight = %d, want exactly %d", maxInFlight, capacity)
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
