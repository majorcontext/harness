package harness

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
)

// births remembers the creation time of each session, which never changes, so
// a page reads the first record only of a session that it has not seen.
type births struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (b *births) get(id string) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	at, ok := b.at[id]
	return at, ok
}

func (b *births) keep(at map[string]time.Time) {
	b.mu.Lock()
	b.at = at
	b.mu.Unlock()
}

// sessionsByCreation returns up to limit session IDs that follow after in
// creation order. The creation time of a session is the time of its first
// record; equal times order by ID. A session whose first record does not
// decode is skipped with a WARN log line. An after that names no session fails.
func (r *Runtime) sessionsByCreation(ctx context.Context, after string, limit int) ([]string, error) {
	type created struct {
		id string
		at time.Time
	}
	var all []created
	seen := map[string]time.Time{}
	cursor := ""
	for {
		ids, err := r.store.Sessions(ctx, cursor, 1000)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			at, ok := r.births.get(id)
			if !ok {
				recs, err := r.store.Read(ctx, id, 0, 1)
				if err != nil {
					return nil, err
				}
				if len(recs) == 0 {
					continue
				}
				env, err := eventlog.Decode(recs[0].Data)
				if err != nil {
					slog.Warn("harness: session skipped in the list", "session", id, "err", err)
					continue
				}
				at = env.Time
			}
			seen[id] = at
			all = append(all, created{id, at})
		}
		if len(ids) < 1000 {
			break
		}
		cursor = ids[len(ids)-1]
	}
	r.births.keep(seen)
	slices.SortFunc(all, func(a, b created) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.id, b.id))
	})
	start := 0
	if after != "" {
		i := slices.IndexFunc(all, func(c created) bool { return c.id == after })
		if i < 0 {
			return nil, fmt.Errorf("%w: after names no session", ErrInvalidRequest)
		}
		start = i + 1
	}
	rest := all[start:]
	if len(rest) > limit {
		rest = rest[:limit]
	}
	var out []string
	for _, c := range rest {
		out = append(out, c.id)
	}
	return out, nil
}
