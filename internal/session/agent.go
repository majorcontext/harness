package session

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// turnTools returns the tools and the source of turn id. A harness-loop
// turn of an agent gets the agent tools, and its source retains each result
// after the hooks.
func (a *Actor) turnTools(id string, ownsLoop bool) ([]turn.Tool, turn.Source) {
	tools, src := a.cfg.Tools, a.cfg.Source
	if !ownsLoop && a.cfg.Agent != nil {
		tools = slices.Concat(tools, a.cfg.Agent, []turn.Tool{toolresult.NewTool(a)})
		src = agentSource{src: src, a: a, turnID: id}
	}
	return turn.Restrict(tools, a.state.AllowedTools()), src
}

// agentSource wraps the source of an agent turn.
type agentSource struct {
	src    turn.Source
	a      *Actor
	turnID string
}

func (s agentSource) Toolset(ctx context.Context, history []eventlog.Message, allowed []string, model string) turn.Toolset {
	var ts turn.Toolset
	if s.src != nil {
		ts = s.src.Toolset(ctx, history, allowed, model)
	}
	ts.Hooks = retainHooks{ts.Hooks, s}
	return ts
}

// retainHooks retain a result after the other hooks, so the blob holds the
// text that the model would see.
type retainHooks struct {
	turn.Hooks
	s agentSource
}

func (h retainHooks) Before(ctx context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	if h.Hooks == nil {
		return c, ""
	}
	return h.Hooks.Before(ctx, c)
}

func (h retainHooks) After(ctx context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	if h.Hooks != nil {
		r = h.Hooks.After(ctx, c, r)
	}
	return h.s.a.retain(h.s.turnID, c.Name, r)
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
	return a.cfg.Blobs.GetBlob(ctx, key)
}

// retain keeps a result above the inline limit out of the history: it
// writes the masked text to a blob, appends tool_result.retained, and
// returns the preview. A result that fits once masked stays inline. A
// failed write keeps the whole result: a result is better than none. The
// blob is written outside the actor, as SaveState writes its blob.
func (a *Actor) retain(turnID, tool string, res protocol.ToolResult) protocol.ToolResult {
	if tool == toolresult.ToolName || len(res.Text) <= toolresult.Inline {
		return res
	}
	masked := toolresult.Mask(res.Text)
	if len(masked) <= toolresult.Inline {
		res.Text = masked
		return res
	}
	var m *toolresult.Meta
	err := a.onTurn(turnID, func() error {
		used := 0
		for _, r := range a.state.Retained() {
			used += r.Bytes
		}
		if used+len(masked) <= toolresult.Budget {
			handle := toolresult.Handle(len(a.state.Retained()) + 1)
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
	if err := a.cfg.Blobs.PutBlob(a.cfg.Base, m.Key, strings.NewReader(masked)); err != nil {
		return res
	}
	err = a.onTurn(turnID, func() error {
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
		rc, err := a.cfg.Blobs.GetBlob(a.cfg.Base, m.Key)
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
