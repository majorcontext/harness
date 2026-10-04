package session

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// source returns the Source of turn r: the Source of the session, then the
// tools of the actor.
func (a *Actor) source(r *running) turn.Source {
	var srcs turn.Sources
	if a.cfg.Source != nil {
		srcs = append(srcs, a.cfg.Source)
	}
	return append(srcs, actorTools{a, r})
}

// actorTools are the tools that read the session itself. A turn of a backend
// that owns its loop gets the history tool, which no allow list hides. A
// harness-loop turn gets read_tool_result and retains each result after the
// other hooks, so the blob holds the text that the model would see, unless
// Config.Retain is off or the allowed tools omit read_tool_result.
type actorTools struct {
	a *Actor
	r *running
}

func (s actorTools) Toolset(_ context.Context, _ []eventlog.Message, allowed []string, model string) turn.Toolset {
	if s.a.cfg.Backend.Capabilities(model).OwnsLoop {
		return turn.Toolset{Tools: []turn.Tool{historyTool{s.a}}}
	}
	if !s.a.cfg.Retain {
		return turn.Toolset{}
	}
	tools := turn.Restrict([]turn.Tool{toolresult.NewTool(s.a)}, allowed)
	if len(tools) == 0 {
		return turn.Toolset{}
	}
	return turn.Toolset{Tools: tools, Hooks: s}
}

func (actorTools) Before(_ context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	return c, ""
}

func (s actorTools) After(_ context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
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

// retain keeps a result above the inline limit out of the history: it
// writes the masked text to a blob, appends tool_result.retained, and
// returns the preview. A result that fits once masked stays inline. A
// failed write keeps the whole result: a result is better than none. The
// blob is written outside the actor, as SaveState writes its blob.
func (a *Actor) retain(run *running, tool string, res protocol.ToolResult) protocol.ToolResult {
	if tool == toolresult.ToolName || len(res.Text) <= toolresult.Inline {
		return res
	}
	masked := toolresult.Mask(res.Text)
	if len(masked) <= toolresult.Inline {
		res.Text = masked
		return res
	}
	var m *toolresult.Meta
	err := a.onRun(run, func() error {
		used, last := 0, 0
		for _, r := range a.state.Retained() {
			used += r.Bytes
			if n, _ := toolresult.Number(r.Handle); n > last {
				last = n
			}
		}
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
	err = a.onRun(run, func() error {
		return a.append(eventlog.ToolResultRetained{Handle: m.Handle, Tool: m.Tool, BlobKey: m.Key, Bytes: m.Bytes, Lines: m.Lines, Head: m.Head})
	})
	if err == nil {
		res.Text = toolresult.Preview(*m, masked)
	}
	return res
}

// indexed adds the index of the retained results to a compaction summary,
// so that a handle stays reachable after the preview that named it folds.
func (a *Actor) indexed(summary string, metas []toolresult.Meta) string {
	index := toolresult.Index(metas, func(m toolresult.Meta) bool {
		rc, err := a.cfg.Store.GetBlob(a.cfg.Base, m.Key)
		if err == nil {
			_ = rc.Close()
		}
		return err == nil
	})
	if summary == "" || index == "" {
		return summary
	}
	return summary + "\n\n" + index
}
