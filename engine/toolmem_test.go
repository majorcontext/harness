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
	"time"

	"github.com/majorcontext/harness/message"
)

// Tests for the concurrent-read memory budget (toolmem.go).
//
// The BOUND itself is proved deterministically: the budget's own invariant
// (reserved bytes never exceed the limit) is exact and cheap to check, and
// holds under concurrent reserve/release with the race detector watching.
// The heap measurements at the bottom corroborate that the invariant
// translates into real memory, and are skipped under -short because they
// allocate hundreds of MB.

// ---- the invariant ----

// TestReadBudgetNeverExceedsItsLimit hammers the budget from many
// goroutines with mixed reservation sizes and checks the one thing that
// must always hold.
func TestReadBudgetNeverExceedsItsLimit(t *testing.T) {
	const limit = 1 << 20
	b := newToolReadBudget(limit)

	var peak int64
	var mu sync.Mutex
	observe := func() {
		got := b.inFlight()
		mu.Lock()
		if got > peak {
			peak = got
		}
		mu.Unlock()
		if got > limit {
			t.Errorf("in-flight %d exceeds the %d-byte limit", got, limit)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := int64(1 << uint(10+i%11)) // 1KB .. 1MB
			release, err := b.reserve(context.Background(), n)
			if err != nil {
				t.Errorf("reserve(%d): %v", n, err)
				return
			}
			observe()
			release()
		}(i)
	}
	wg.Wait()

	if got := b.inFlight(); got != 0 {
		t.Errorf("in-flight = %d after every reservation released, want 0 (leak)", got)
	}
	if peak == 0 {
		t.Error("never observed a non-zero reservation; the test did not exercise the budget")
	}
	t.Logf("peak observed in-flight: %d of %d", peak, limit)
}

// TestReadBudgetSerializesOversizedReservations proves the budget actually
// blocks: three reservations of half the limit each cannot all hold at
// once, so the third waits.
func TestReadBudgetSerializesOversizedReservations(t *testing.T) {
	const limit = 1000
	b := newToolReadBudget(limit)

	r1, err := b.reserve(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := b.reserve(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.inFlight(); got != 1000 {
		t.Fatalf("in-flight = %d, want 1000", got)
	}

	third := make(chan struct{})
	go func() {
		r3, err := b.reserve(context.Background(), 500)
		if err == nil {
			r3()
		}
		close(third)
	}()

	select {
	case <-third:
		t.Fatal("the third reservation was admitted while the budget was full")
	case <-time.After(50 * time.Millisecond):
	}

	r1()
	select {
	case <-third:
	case <-time.After(2 * time.Second):
		t.Fatal("the third reservation never ran after a release freed room")
	}
	r2()
	if got := b.inFlight(); got != 0 {
		t.Errorf("in-flight = %d, want 0", got)
	}
}

// TestReadBudgetServesWaitersFIFO is the anti-starvation property. A large
// reservation queued behind a full budget must be served before a small
// one that arrives after it — a "retry when there is room" loop would let
// a stream of small reads starve the large one forever.
func TestReadBudgetServesWaitersFIFO(t *testing.T) {
	const limit = 100
	b := newToolReadBudget(limit)

	hold, err := b.reserve(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	note := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	// Queue the big one first, then the small one. Both are queued before
	// anything is released, so the order they are SERVED in is the
	// budget's choice, not a scheduling artifact.
	queued := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(queued)
		r, err := b.reserve(context.Background(), 100)
		if err != nil {
			t.Error(err)
			return
		}
		note("big")
		r()
	}()
	<-queued
	waitForWaiters(t, b, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		r, err := b.reserve(context.Background(), 1)
		if err != nil {
			t.Error(err)
			return
		}
		note("small")
		r()
	}()
	waitForWaiters(t, b, 2)

	hold()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "big" {
		t.Errorf("served %v, want the queued big reservation first (FIFO, no starvation)", order)
	}
}

// waitForWaiters blocks until the budget has at least n queued waiters, so
// a test can order its own queue deterministically instead of sleeping.
func waitForWaiters(t *testing.T, b *toolReadBudget, n int) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		b.mu.Lock()
		got := len(b.waiters)
		b.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d queued waiters", n)
}

// TestReadBudgetClampsAnOversizedReservation checks a single read larger
// than the WHOLE budget still runs, alone, rather than deadlocking the
// batch forever.
func TestReadBudgetClampsAnOversizedReservation(t *testing.T) {
	b := newToolReadBudget(1000)
	release, err := b.reserve(context.Background(), 1<<30)
	if err != nil {
		t.Fatalf("an over-budget reservation must be admitted alone, got %v", err)
	}
	if got := b.inFlight(); got != 1000 {
		t.Errorf("in-flight = %d, want the whole budget (1000) reserved", got)
	}
	release()
	if got := b.inFlight(); got != 0 {
		t.Errorf("in-flight = %d after release, want 0", got)
	}
}

// TestReadBudgetCancelWhileQueuedReleasesNothing checks a reservation
// abandoned because its turn was cancelled neither leaks bytes nor leaves
// a stale waiter behind.
func TestReadBudgetCancelWhileQueuedReleasesNothing(t *testing.T) {
	b := newToolReadBudget(100)
	hold, err := b.reserve(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		release, err := b.reserve(ctx, 50)
		if err == nil {
			release()
		}
		errc <- err
	}()
	waitForWaiters(t, b, 1)
	cancel()

	if err := <-errc; err == nil {
		t.Error("reserve returned nil error after its context was cancelled")
	}

	hold()
	if got := b.inFlight(); got != 0 {
		t.Errorf("in-flight = %d after the cancelled waiter left, want 0", got)
	}
	b.mu.Lock()
	n := len(b.waiters)
	b.mu.Unlock()
	if n != 0 {
		t.Errorf("%d waiters still queued after cancellation, want 0", n)
	}
}

// TestReadBudgetDisabledAndDefault pins the config resolution.
func TestReadBudgetDisabledAndDefault(t *testing.T) {
	if b := newToolReadBudget(-1); b != nil {
		t.Error("a negative budget must disable the bound (nil)")
	}
	// A nil budget must still be usable, so no caller needs to branch.
	var nilBudget *toolReadBudget
	release, err := nilBudget.reserve(context.Background(), 1<<40)
	if err != nil {
		t.Fatalf("nil budget must reserve freely: %v", err)
	}
	release()
	if got := newToolReadBudget(0).limit; got != defaultToolReadBudgetBytes {
		t.Errorf("unset budget = %d, want the package default %d", got, defaultToolReadBudgetBytes)
	}
	if got := newToolReadBudget(4096).limit; got != 4096 {
		t.Errorf("explicit budget = %d, want 4096", got)
	}
}

// ---- the concurrency win must survive ----

// TestReadBudgetKeepsSmallReadsFullyParallel is the regression guard for
// the fix itself: bounding memory must not serialize ordinary work. Eight
// kilobyte-sized reservations against the default budget must ALL be held
// at once, with nothing queued.
func TestReadBudgetKeepsSmallReadsFullyParallel(t *testing.T) {
	b := newToolReadBudget(0) // the default
	var releases []func()
	for i := 0; i < 8; i++ {
		release, err := b.reserve(context.Background(), 64<<10) // 64KB, a large source file
		if err != nil {
			t.Fatalf("reservation %d blocked or failed: %v", i, err)
		}
		releases = append(releases, release)
	}
	b.mu.Lock()
	queued := len(b.waiters)
	b.mu.Unlock()
	if queued != 0 {
		t.Errorf("%d ordinary-size reservations queued; the budget must not serialize normal work", queued)
	}
	if got, want := b.inFlight(), int64(8*64<<10); got != want {
		t.Errorf("in-flight = %d, want %d (all eight held at once)", got, want)
	}
	for _, r := range releases {
		r()
	}
}

// TestReadBudgetBoundsARealBatch drives real read_file calls through the
// executor and samples the budget while the batch runs, so the bound is
// observed end to end rather than only at the unit level.
func TestReadBudgetBoundsARealBatch(t *testing.T) {
	const n = 8
	const size = 1 << 20 // 1MB each
	const limit = 2 << 20

	dir := t.TempDir()
	body := strings.Repeat(strings.Repeat("z", 127)+"\n", size/128)
	var calls []*message.ToolCall
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, &message.ToolCall{
			CallID:    fmt.Sprintf("r%d", i),
			Name:      "read_file",
			Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, p)),
		})
	}

	s := NewSession(Config{WorkDir: dir, ToolConcurrency: 8, ToolReadBudgetBytes: limit})

	var over atomic.Int64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if got := s.readBudget.inFlight(); got > limit {
				over.Store(got)
			}
		}
	}()

	results := s.runToolCalls(context.Background(), asstWith(calls...))
	close(stop)
	<-stopped

	wantOrder(t, results, calls...)
	for i, p := range results {
		tr := p.(*message.ToolResult)
		if tr.IsError {
			t.Fatalf("read %d errored: %s", i, resultText(tr.Content))
		}
	}
	if got := over.Load(); got != 0 {
		t.Errorf("observed %d bytes in flight, over the %d-byte budget", got, limit)
	}
	if got := s.readBudget.inFlight(); got != 0 {
		t.Errorf("in-flight = %d after the batch, want 0 (leak)", got)
	}
}

// ---- heap corroboration ----

// TestReadBudgetBoundsPeakHeap measures what the invariant buys. It is the
// regression this whole file exists for: without a budget, eight
// concurrent large reads hold eight working sets at once.
//
// Skipped under -short: it allocates hundreds of MB by design. Heap
// sampling is inherently noisy, so the assertions are deliberately loose —
// the exact bound is proved by the invariant tests above, not here.
func TestReadBudgetBoundsPeakHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates hundreds of MB")
	}
	const n = 8
	const size = 16 << 20

	dir := t.TempDir()
	body := strings.Repeat(strings.Repeat("y", 127)+"\n", size/128)
	var calls []*message.ToolCall
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("big%d.txt", i))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, &message.ToolCall{
			CallID:    fmt.Sprintf("r%d", i),
			Name:      "read_file",
			Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, p)),
		})
	}

	// Retention on with a tiny inline limit, so each final result collapses
	// to a preview and the accumulated term (identical in both execution
	// modes, and not what this budget bounds) drops out of the measurement.
	measure := func(concurrency int, budget int64) uint64 {
		sess := t.TempDir()
		runtime.GC()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)
		var peak uint64
		stop, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(stopped)
			for {
				select {
				case <-stop:
					return
				default:
				}
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak {
					peak = m.HeapAlloc
				}
				time.Sleep(time.Millisecond)
			}
		}()
		s := NewSession(Config{
			WorkDir:               dir,
			SessionDir:            sess,
			ToolResultInlineBytes: 500,
			ToolConcurrency:       concurrency,
			ToolReadBudgetBytes:   budget,
		})
		res := s.runToolCalls(context.Background(), asstWith(calls...))
		close(stop)
		<-stopped
		wantOrder(t, res, calls...)
		if peak < base.HeapAlloc {
			return 0
		}
		return peak - base.HeapAlloc
	}

	unbounded := measure(8, -1)  // the pre-fix behavior
	bounded := measure(8, size)  // budget of one file
	sequential := measure(1, -1) // the implicit bound concurrency removed

	mb := func(b uint64) float64 { return float64(b) / (1 << 20) }
	t.Logf("%d x %d MB files", n, size>>20)
	t.Logf("parallel, budget DISABLED: %.0f MB", mb(unbounded))
	t.Logf("parallel, budget %d MB:     %.0f MB", size>>20, mb(bounded))
	t.Logf("sequential (cap 1):        %.0f MB", mb(sequential))
	if sequential > 0 {
		t.Logf("amplification without the budget: %.1fx; with it: %.1fx",
			float64(unbounded)/float64(sequential), float64(bounded)/float64(sequential))
	}

	if bounded >= unbounded {
		t.Errorf("budget did not reduce peak heap: bounded %.0f MB vs unbounded %.0f MB", mb(bounded), mb(unbounded))
	}
	if sequential > 0 && float64(bounded) > 3*float64(sequential) {
		t.Errorf("bounded peak %.0f MB is more than 3x the sequential %.0f MB; the budget is not holding",
			mb(bounded), mb(sequential))
	}
}

// resultText joins a result's Text parts, for error messages.
func resultText(parts message.Parts) string {
	var b strings.Builder
	for _, p := range parts {
		if txt, ok := p.(*message.Text); ok {
			b.WriteString(txt.Text)
		}
	}
	return b.String()
}
