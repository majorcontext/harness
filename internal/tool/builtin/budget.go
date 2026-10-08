package builtin

import (
	"context"
	"sync"
)

// readBudgetBytes bounds the estimated bytes that the file calls of one
// session hold at once. The cap on one file alone lets eight calls hold
// 160 MiB of raw bytes, and a read also holds a numbered copy.
const readBudgetBytes = 64 << 20

// budget hands out byte reservations in the order that they ask. A
// reservation above the limit takes the whole limit, so the call waits for
// the others and then runs alone.
type budget struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	waiters []*reservation
}

type reservation struct {
	n       int64
	granted bool
	ready   chan struct{}
}

// reserve waits for n bytes and returns the release of them. It returns the
// error of ctx when ctx ends first, with nothing reserved.
func (b *budget) reserve(ctx context.Context, n int64) (func(), error) {
	if n <= 0 {
		return func() {}, nil
	}
	n = min(n, b.limit)
	b.mu.Lock()
	if len(b.waiters) == 0 && b.used+n <= b.limit {
		b.used += n
		b.mu.Unlock()
		return b.releaser(n), nil
	}
	w := &reservation{n: n, ready: make(chan struct{})}
	b.waiters = append(b.waiters, w)
	b.mu.Unlock()
	select {
	case <-w.ready:
		return b.releaser(n), nil
	case <-ctx.Done():
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
			b.release(n)
		}
		return nil, context.Cause(ctx)
	}
}

func (b *budget) releaser(n int64) func() {
	var once sync.Once
	return func() { once.Do(func() { b.release(n) }) }
}

func (b *budget) release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	for len(b.waiters) > 0 {
		w := b.waiters[0]
		if b.used+w.n > b.limit {
			return
		}
		b.used += w.n
		w.granted = true
		b.waiters = b.waiters[1:]
		close(w.ready)
	}
}
