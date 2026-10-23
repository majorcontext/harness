package session

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// source returns the Source of turn r: the Source of the session, then the
// tools of the actor. A nil r is the next turn, for a warm-up that runs no tool.
func (a *Actor) source(r *running) turn.Source {
	var srcs turn.Sources
	if a.cfg.Source != nil {
		srcs = append(srcs, a.cfg.Source)
	}
	srcs = append(srcs, actorTools{a, r})
	if a.cfg.Record != nil {
		return a.cfg.Record(srcs)
	}
	return srcs
}

// actorTools are the tools that read the session itself. A turn of a backend
// that owns its loop gets the history tool, which no allow list hides. A
// turn gets ask_user_question when it may ask a question, and
// read_tool_result with the retention of each result in call order, after the
// other hooks, so the blob holds the text that the model would see, unless
// Config.Retain is off or the allowed tools omit read_tool_result.
type actorTools struct {
	a *Actor
	r *running
}

func (s actorTools) Toolset(ctx context.Context, _ []protocol.LogMessage, allowed []string, model string) turn.Toolset {
	var ts turn.Toolset
	if s.a.cfg.Backend.Capabilities(model).OwnsLoop {
		ts.Tools = []turn.Tool{historyTool{s.a}}
	}
	if s.mayAsk(ctx) {
		ts.Tools = append(ts.Tools, turn.Restrict([]turn.Tool{turn.NewAskTool()}, allowed)...)
	}
	if s.a.cfg.Retain {
		if tools := turn.Restrict([]turn.Tool{toolresult.NewTool(s.a)}, allowed); len(tools) > 0 {
			ts.Tools, ts.Hooks = append(ts.Tools, tools...), s
		}
	}
	return ts
}

// mayAsk reports whether the session may ask a question now. A goal that
// becomes active during a turn takes the permission away for the later model calls.
func (s actorTools) mayAsk(ctx context.Context) bool {
	ok, _ := call(ctx, s.a, func(reply func(bool, error)) { reply((s.r == nil || s.a.run == s.r) && s.a.questions(), nil) })
	return ok
}

func (actorTools) Before(_ context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	return c, ""
}

func (actorTools) After(_ context.Context, _ protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	return r
}

func (s actorTools) Join(_ context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	return s.a.retain(s.r, c.Name, r)
}

// Retained returns the retained tool results of the session.
func (a *Actor) Retained(ctx context.Context) ([]toolresult.Meta, error) {
	return call(ctx, a, func(reply func([]toolresult.Meta, error)) { reply(a.retained(), nil) })
}

func (a *Actor) retained() []toolresult.Meta {
	var out []toolresult.Meta
	for _, r := range a.state.Retained() {
		out = append(out, toolresult.Meta{Handle: r.Handle, Tool: r.Tool, Key: r.BlobKey, Bytes: r.Bytes, Lines: r.Lines, Head: r.Head})
	}
	return out
}

// Open opens a blob of the session.
func (a *Actor) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return a.cfg.Store.GetBlob(ctx, key)
}

// blob returns the bytes of a blob of the session: the attachment of a blob part.
func (a *Actor) blob(ctx context.Context, key string) ([]byte, error) {
	return readBlob(ctx, a.cfg.Store, key)
}

func readBlob(ctx context.Context, st Blobs, key string) ([]byte, error) {
	rc, err := st.GetBlob(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// retain keeps a result above the inline limit out of the history: it
// writes the masked text to a blob, appends tool_result.retained, and
// returns the preview. A result that fits once masked stays inline. A
// failed write keeps the whole result: a result is better than none. The
// blob is written outside the actor, as SaveState writes its blob.
func (a *Actor) retain(run *running, tool string, res protocol.ToolResult) protocol.ToolResult {
	if tool == toolresult.ToolName || tool == mcpsrc.ToolName || len(res.Text) <= toolresult.Inline {
		return res
	}
	masked := toolresult.Mask(res.Text)
	if len(masked) <= toolresult.Inline {
		res.Text = masked
		return res
	}
	var m *toolresult.Meta
	err := a.onRun(run, func() error {
		used, last := a.retainedTotals()
		if used+len(masked) <= toolresult.Budget {
			handle := toolresult.Handle(last + 1)
			m = new(toolresult.NewMeta(handle, tool, fmt.Sprintf("%s-%d", handle, a.fenced), masked))
		}
		return nil
	})
	switch {
	case err != nil:
		return res
	case m == nil:
		res.Text = toolresult.Refused(tool, masked)
		return res
	}
	if err := a.cfg.Store.PutBlob(a.cfg.Base, m.Key, strings.NewReader(masked)); err != nil {
		return res
	}
	refused := false
	err = a.onRun(run, func() error {
		used, last := a.retainedTotals()
		if used+len(masked) > toolresult.Budget {
			refused = true
			return nil
		}
		if handleNumber(m.Handle) <= last {
			m = new(toolresult.NewMeta(toolresult.Handle(last+1), tool, m.Key, masked))
		}
		return a.append(protocol.ToolResultRetained{Handle: m.Handle, Tool: m.Tool, BlobKey: m.Key, Bytes: m.Bytes, Lines: m.Lines, Head: m.Head})
	})
	switch {
	case err != nil:
	case refused:
		res.Text = toolresult.Refused(tool, masked)
	default:
		res.Text = toolresult.Preview(*m, masked)
	}
	return res
}

// resultIndex returns the index of the retained results for a compaction,
// so that a handle stays reachable after the preview that named it folds.
func (a *Actor) resultIndex(metas []toolresult.Meta) string {
	index := toolresult.Index(metas, func(m toolresult.Meta) bool {
		rc, err := a.cfg.Store.GetBlob(a.cfg.Base, m.Key)
		if err == nil {
			_ = rc.Close()
		}
		return err == nil
	})
	return index
}
