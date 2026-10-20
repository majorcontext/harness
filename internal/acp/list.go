package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// listStep is the number of sessions that session/list reads from the store at
// once.
const listStep = 50

// listSteps is the most steps that one session/list reads. A call that reaches
// it with no match answers an empty page and a cursor.
const listSteps = 4

// sessionInfo is one entry of the answer of session/list. The title is null
// while the session has none.
type sessionInfo struct {
	SessionID string  `json:"sessionId"`
	CWD       string  `json:"cwd"`
	Title     *string `json:"title"`
	UpdatedAt string  `json:"updatedAt"`
}

type listResult struct {
	Sessions   []sessionInfo `json:"sessions"`
	NextCursor *string       `json:"nextCursor,omitempty"`
}

// listSessions lists the root sessions of the store in creation order. A task
// child is not listed, because a client cannot use it without its parent. A
// session with no work directory is not listed, because the entry needs a cwd.
// The cursor is the ID of the last session that the previous page read.
func (a *agent) listSessions(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		CWD    *string `json:"cwd"`
		Cursor *string `json:"cursor"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, invalidParams("cwd and cursor are strings")
		}
	}
	var cwd string
	if p.CWD != nil {
		if !filepath.IsAbs(*p.CWD) {
			return nil, invalidParams("cwd must be an absolute path")
		}
		cwd = filepath.Clean(*p.CWD)
	}
	rt, err := a.listRuntime(ctx, cwd)
	if err != nil {
		return nil, err
	}
	res := listResult{Sessions: []sessionInfo{}}
	after := ""
	if p.Cursor != nil {
		after = *p.Cursor
	}
	if rt == nil {
		return res, nil
	}
	for range listSteps {
		page, err := rt.List(ctx, protocol.ListSessions{After: after, Limit: listStep})
		if err != nil {
			return nil, rpcError(err)
		}
		for _, s := range page.Sessions {
			if s.ParentID != "" || s.WorkDir == "" || (cwd != "" && filepath.Clean(s.WorkDir) != cwd) {
				continue
			}
			info := sessionInfo{SessionID: s.ID, CWD: s.WorkDir, UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339)}
			if s.Title != "" {
				info.Title = &s.Title
			}
			res.Sessions = append(res.Sessions, info)
		}
		if page.Next == "" {
			return res, nil
		}
		after = page.Next
		if len(res.Sessions) > 0 {
			break
		}
	}
	res.NextCursor = &after
	return res, nil
}

// listRuntime returns the Runtime that reads the store for a list, or nil when
// a list with no cwd finds none.
func (a *agent) listRuntime(ctx context.Context, cwd string) (Runtime, error) {
	if a.isClosed() {
		return nil, rpcError(harness.ErrSessionReleased)
	}
	if a.rts == nil {
		return nil, rpcError(errNoRuntimes)
	}
	var rt Runtime
	var err error
	if cwd == "" {
		if rt, err = a.rts.Default(ctx, a.runtimeOptions()); err == nil && rt == nil {
			return nil, nil
		}
	} else {
		rt, err = a.rts.For(ctx, cwd, a.runtimeOptions())
	}
	if err != nil {
		return nil, rpcError(err)
	}
	return rt, nil
}
