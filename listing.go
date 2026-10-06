package harness

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/protocol"
)

// listSessions returns a page of sessions in creation order. describe reads
// the view of one session; a session that does not replay is skipped with a
// WARN log line.
func listSessions(ctx context.Context, st Store, b *births, q protocol.ListSessions, describe func(context.Context, string) (protocol.Session, error)) (protocol.SessionPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	ids, err := sessionsByCreation(ctx, st, b, q.After, limit)
	if err != nil {
		return protocol.SessionPage{}, err
	}
	page := protocol.SessionPage{Sessions: []protocol.Session{}}
	for _, id := range ids {
		v, err := describe(ctx, id)
		if errors.Is(err, session.ErrUnreplayable) {
			slog.Warn("harness: session skipped in the list", "session", id, "err", err)
			continue
		}
		if err != nil {
			return protocol.SessionPage{}, err
		}
		page.Sessions = append(page.Sessions, v)
	}
	if len(ids) == limit {
		page.Next = ids[len(ids)-1]
	}
	return page, nil
}

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
func sessionsByCreation(ctx context.Context, st Store, b *births, after string, limit int) ([]string, error) {
	type created struct {
		id string
		at time.Time
	}
	var all []created
	seen := map[string]time.Time{}
	cursor := ""
	for {
		ids, err := st.Sessions(ctx, cursor, 1000)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			at, ok := b.get(id)
			if !ok {
				recs, err := st.Read(ctx, id, 0, 1)
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
	b.keep(seen)
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
