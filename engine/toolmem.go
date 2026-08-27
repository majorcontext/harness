package engine

import (
	"context"
	"sync"
)

// Bounding the memory a batch of concurrent tool calls holds at once.
//
// # The problem
//
// read_file's TEXT path is an unbounded io.ReadAll (readPathContent,
// filetools.go): a coding agent legitimately reads a whole file, and no
// byte cap would be right for every file. Only the IMAGE path is capped
// (readFileMaxImageBytes), because an oversized image is useless to a
// model, and bash caps its own output (defaultBashOutputCap).
//
// That was safe while tool calls ran strictly one at a time: peak heap
// held at most ONE file's raw bytes plus the line-numbered copy built
// from them. The concurrent executor (toolexec.go) removed that implicit
// bound without replacing it, so a batch of N large reads holds N of
// those working sets at once. Measured with eight 16MB files, retention
// swallowing the finals so only the transient term shows (see
// TestReadBudgetBoundsPeakHeap): ~325MB peak parallel against ~73MB
// sequential, a ~4.3x amplification bounded only by ToolConcurrency. A
// wider cap multiplies it further, and the model chooses both the batch
// width and the file sizes. With the budget set to one file's size the
// same batch peaks at the sequential figure — 1.0x.
//
// # What this bounds, and what it does not
//
// This budget bounds the TRANSIENT working set: bytes a read holds while
// it is in progress. That is the parallel-specific term, and the one the
// measurement above isolates.
//
// It deliberately does NOT bound the ACCUMULATED results. runToolBatch
// holds every call's output until the join in BOTH execution modes, so
// eight 16MB results occupy the same memory whether they were produced
// concurrently or one after another. That term is not a regression from
// concurrency and bounding it would mean changing what a batch of reads
// returns, not when. Retention (toolresult.go) already collapses
// oversized results where it is configured.
//
// edit_file's whole-file rewrite and write_file's read-guard hash remain
// outside this budget; bounding them is deferred because this change scopes
// reservations to read_file without changing either tool's I/O contract.
// A non-regular file whose Stat size is zero also reserves nothing; bounding
// that path is deferred until read_file has a byte cap for streams whose size
// cannot be estimated safely.
//
// # Why a byte budget rather than a count
//
// The hazard is the PRODUCT of read size and concurrency, so bounding
// either factor alone misses. A limit of "two large reads at once" still
// admits two 500MB reads; a limit on file size breaks the legitimate
// large read this tool exists to serve. Reserving estimated bytes bounds
// the product directly: eight concurrent 100KB reads all proceed at once
// (they fit), while eight concurrent 64MB reads serialize down to what
// fits. Normal-size tool calls never contend, so the concurrency win is
// untouched — TestReadBudgetKeepsSmallReadsFullyParallel pins that.
//
// The reservation is an ESTIMATE, taken from the open file handle's own
// Stat size. Correctness does not depend on it being exact: a file that
// grows after the reservation overshoots the budget by the growth, which
// is bounded by what the process can write in the meantime, and a file
// that shrinks merely over-reserves. This is why the estimate may use a
// Stat size where readPathContent's image CAP deliberately may not — a
// cap that binds on bytes actually read is a correctness boundary, while
// this is an admission hint.
//
// # Ordering and deadlock
//
// A worker takes its pool slot first (toolexec.go), then reserves. That
// is head-of-line blocking by construction: a worker can sit on a slot
// while it waits for budget. It cannot deadlock. Only a slot holder ever
// holds budget, so whenever anyone is waiting at least one holder is
// doing I/O and will release; a reservation is never held while acquiring
// another. A single read larger than the WHOLE budget is clamped to the
// budget rather than refused, so it waits for the budget to drain and
// then runs alone: a batch can always make progress.
//
// Waiters are served strictly FIFO. A plain "retry when there is room"
// loop lets a stream of small reads starve one large one indefinitely;
// with FIFO, a queued large read blocks later small ones rather than
// yielding to them forever.
type toolReadBudget struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	waiters []*readBudgetWaiter

	// onCancel is a test seam fired after a queued reserve selects ctx.Done
	// and before it reacquires mu. Set before use and never changed.
	onCancel func()
}

// readBudgetWaiter is one queued reservation. granted is set under the
// budget's mutex at the moment the waiter is handed its bytes, so a
// reservation racing its own context cancellation can tell whether it
// owns bytes it must give back.
type readBudgetWaiter struct {
	n       int64
	granted bool
	ready   chan struct{}
}

// defaultToolReadBudgetBytes is Config.ToolReadBudgetBytes' zero-value
// default: the total estimated bytes a session's in-flight tool reads may
// hold at once.
//
// Chosen so ordinary work never contends. Source files are kilobytes, so
// a full-width batch of them reserves a rounding error against this and
// runs fully parallel; only genuinely large reads (multiple MB) ever
// queue. With the roughly 2.5x expansion a line-numbered copy adds on top
// of the raw bytes, this bounds the transient term at a few hundred MB
// even when every slot holds a large read — well inside a container
// memory limit, where the unbounded behavior was not.
const defaultToolReadBudgetBytes int64 = 64 << 20 // 64 MiB

// newToolReadBudget resolves Config.ToolReadBudgetBytes. Zero (unset)
// takes the package default; a negative value disables the budget and
// yields nil, which every method below treats as unlimited.
func newToolReadBudget(configured int64) *toolReadBudget {
	switch {
	case configured < 0:
		return nil
	case configured == 0:
		return &toolReadBudget{limit: defaultToolReadBudgetBytes}
	default:
		return &toolReadBudget{limit: configured}
	}
}

// reserve blocks until n estimated bytes are available, then returns a
// release func the caller MUST call when the bytes are no longer held.
//
// A nil budget, a non-positive limit, or a non-positive n reserves
// nothing and returns a no-op release, so a caller never needs to know
// whether the budget is enabled. n above the whole limit is clamped to
// the limit rather than refused (see the ordering note above).
//
// It returns ctx.Err() only when ctx ends while queued. The returned
// release is nil in that case and must not be called; the caller returns
// an error result for its own call instead, which keeps the
// one-result-per-tool-call invariant intact.
func (b *toolReadBudget) reserve(ctx context.Context, n int64) (func(), error) {
	if b == nil || b.limit <= 0 || n <= 0 {
		return func() {}, nil
	}
	if n > b.limit {
		n = b.limit
	}

	b.mu.Lock()
	// Jump the queue only when it is empty: taking bytes ahead of an
	// already-waiting reservation is what starves a large read.
	if len(b.waiters) == 0 && b.used+n <= b.limit {
		b.used += n
		b.mu.Unlock()
		return b.releaser(n), nil
	}
	w := &readBudgetWaiter{n: n, ready: make(chan struct{})}
	b.waiters = append(b.waiters, w)
	b.mu.Unlock()

	select {
	case <-w.ready:
		return b.releaser(n), nil
	case <-ctx.Done():
		if b.onCancel != nil {
			b.onCancel()
		}
		b.mu.Lock()
		granted := w.granted
		if !granted {
			for i, q := range b.waiters {
				if q == w {
					b.waiters = append(b.waiters[:i], b.waiters[i+1:]...)
					break
				}
			}
		}
		b.mu.Unlock()
		if granted {
			// The grant landed between ctx ending and the lock. The bytes
			// are ours, and nobody will ever call our release, so give
			// them back here.
			b.release(n)
		}
		return nil, ctx.Err()
	}
}

// releaser returns a release func that gives n bytes back exactly once.
func (b *toolReadBudget) releaser(n int64) func() {
	var once sync.Once
	return func() { once.Do(func() { b.release(n) }) }
}

// release returns n bytes and hands them to whichever queued waiters now
// fit, in FIFO order, stopping at the first that does not.
func (b *toolReadBudget) release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
	for len(b.waiters) > 0 {
		w := b.waiters[0]
		if b.used+w.n > b.limit {
			return
		}
		b.used += w.n
		w.granted = true
		b.waiters[0] = nil
		b.waiters = b.waiters[1:]
		close(w.ready)
	}
}

// inFlight reports the currently reserved bytes. For tests and
// diagnostics only.
func (b *toolReadBudget) inFlight() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
