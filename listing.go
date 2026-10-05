package harness

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
)

// sessionsByCreation returns up to limit session IDs that follow after in
// creation order. The creation time of a session is the time of its first
// record; equal times order by ID. An after that names no session fails.
func (r *Runtime) sessionsByCreation(ctx context.Context, after string, limit int) ([]string, error) {
	type created struct {
		id string
		at time.Time
	}
	var all []created
	cursor := ""
	for {
		ids, err := r.store.Sessions(ctx, cursor, 1000)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			recs, err := r.store.Read(ctx, id, 0, 1)
			if err != nil {
				return nil, err
			}
			if len(recs) == 0 {
				continue
			}
			env, err := eventlog.Decode(recs[0].Data)
			if err != nil {
				return nil, fmt.Errorf("session %s: %w", id, err)
			}
			all = append(all, created{id, env.Time})
		}
		if len(ids) < 1000 {
			break
		}
		cursor = ids[len(ids)-1]
	}
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
	var out []string
	for _, c := range all[start:min(len(all), start+limit)] {
		out = append(out, c.id)
	}
	return out, nil
}
