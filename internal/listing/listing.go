// Package listing pages the sessions of a store in creation order.
package listing

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

const (
	defaultLimit = 100
	idPage       = 1000
)

// Source is what a page reads: the IDs of the store, the first record of a
// session, and the view of one session.
type Source struct {
	// Sessions returns up to limit sorted session IDs above after.
	Sessions func(ctx context.Context, after string, limit int) ([]string, error)
	// First returns the data of the first record of session id, or nil when the session has none.
	First func(ctx context.Context, id string) ([]byte, error)
	// Describe reads the view of one session.
	Describe func(ctx context.Context, id string) (protocol.Session, error)
	// Invalid wraps the error of a request that no page can serve.
	Invalid error
	// Log receives a WARN line for each session that the page skips.
	Log *slog.Logger
}

// Lister remembers the creation time of each session, which never changes, so
// a page reads the first record only of a session that it has not seen. The
// zero value is ready; it is safe for concurrent use.
type Lister struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (l *Lister) get(id string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.at[id]
	return at, ok
}

func (l *Lister) keep(at map[string]time.Time) {
	l.mu.Lock()
	l.at = at
	l.mu.Unlock()
}

// Page returns a page of sessions in creation order. describe reads the view
// of one session; a session that does not replay is skipped with a WARN log
// line.
func (l *Lister) Page(ctx context.Context, src Source, q protocol.ListSessions) (protocol.SessionPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	ids, err := l.byCreation(ctx, src, q.After, limit)
	if err != nil {
		return protocol.SessionPage{}, err
	}
	page := protocol.SessionPage{Sessions: []protocol.Session{}}
	for _, id := range ids {
		v, err := src.Describe(ctx, id)
		if errors.Is(err, session.ErrUnreplayable) {
			src.Log.Warn("harness: session skipped in the list", "session", id, "err", err)
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

// byCreation returns up to limit session IDs that follow after in creation
// order. The creation time of a session is the time of its first record;
// equal times order by ID. A session whose first record does not decode is
// skipped with a WARN log line. An after that names no session fails.
func (l *Lister) byCreation(ctx context.Context, src Source, after string, limit int) ([]string, error) {
	type created struct {
		id string
		at time.Time
	}
	var all []created
	seen := map[string]time.Time{}
	cursor := ""
	for {
		ids, err := src.Sessions(ctx, cursor, idPage)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			at, ok := l.get(id)
			if !ok {
				data, err := src.First(ctx, id)
				if err != nil {
					return nil, err
				}
				if data == nil {
					continue
				}
				env, err := eventlog.Decode(data)
				if err != nil {
					src.Log.Warn("harness: session skipped in the list", "session", id, "err", err)
					continue
				}
				at = env.Time
			}
			seen[id] = at
			all = append(all, created{id, at})
		}
		if len(ids) < idPage {
			break
		}
		cursor = ids[len(ids)-1]
	}
	l.keep(seen)
	slices.SortFunc(all, func(a, b created) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.id, b.id))
	})
	start := 0
	if after != "" {
		i := slices.IndexFunc(all, func(c created) bool { return c.id == after })
		if i < 0 {
			return nil, fmt.Errorf("%w: after names no session", src.Invalid)
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
