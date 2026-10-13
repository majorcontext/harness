package plugin

import (
	"context"
	"encoding/json"
	"fmt"

	pub "github.com/majorcontext/harness/plugin"
)

// ToolExecuteBefore runs the tool.execute.before chain. It returns the
// (possibly rewritten) args, or a non-empty deny message that stops the chain
// and the tool. A failed flagged hook denies; a failure under a canceled ctx
// is an abort and denies nothing.
func (h *Host) ToolExecuteBefore(ctx context.Context, req *pub.ToolExecuteBeforeRequest) (args json.RawMessage, deny string) {
	dispatchChain(ctx, h, pub.HookToolExecuteBefore, req, func(req *pub.ToolExecuteBeforeRequest, resp *pub.ToolExecuteBeforeResponse) bool {
		if resp.Deny != "" {
			deny = resp.Deny
			return false
		}
		if resp.Args != nil {
			req.Args = resp.Args
		}
		return true
	}, func(inst *instance, err error) bool {
		if ctx.Err() != nil {
			return true
		}
		if !inst.flags(pub.HookToolExecuteBefore) {
			return false
		}
		deny = fmt.Sprintf("denied: plugin %s failed: %v", inst.spec.Manifest.Name, err)
		return true
	})
	return req.Args, deny
}

// ToolExecuteAfter runs the tool.execute.after chain and returns the final
// output.
func (h *Host) ToolExecuteAfter(ctx context.Context, req *pub.ToolExecuteAfterRequest) []pub.Part {
	dispatchChain(ctx, h, pub.HookToolExecuteAfter, req, func(req *pub.ToolExecuteAfterRequest, resp *toolAfterResponse) bool {
		if resp.Output != nil {
			req.Output = resp.Output
		}
		return true
	}, nil)
	return req.Output
}

// dispatchChain runs one sync hook across subscribed plugins in order. apply
// folds each response into the shared request; returning false stops the
// chain. A failure (including the per-dispatch deadline) skips the plugin and
// continues, so a hook chain fails open and a plugin never wedges a session.
// onFail, when set, sees each failure first: returning true stops the chain.
func dispatchChain[Req, Resp any](ctx context.Context, h *Host, hook pub.Hook, req *Req, apply func(*Req, *Resp) bool, onFail func(inst *instance, err error) (stop bool)) {
	for _, inst := range h.instances {
		if !inst.subscribes(hook) {
			continue
		}
		timeout := inst.hookTimeout(hook, h.opts.HookTimeout)
		sctx, scancel := context.WithTimeout(ctx, max(timeout, h.opts.HookTimeout))
		c, err := inst.start(sctx)
		scancel()
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			var resp Resp
			err = c.Call(cctx, hookMethod(hook), req, &resp)
			cancel()
			if err == nil {
				if !apply(req, &resp) {
					return
				}
				continue
			}
		}
		h.onError(inst, hook, err)
		if onFail != nil && onFail(inst, err) {
			return
		}
	}
}

func (h *Host) onError(inst *instance, hook pub.Hook, err error) {
	if h.opts.OnError != nil {
		h.opts.OnError(inst.spec.Manifest.Name, hook, err)
	}
}

// ChatParams runs the chat.params chain and returns the final params.
func (h *Host) ChatParams(ctx context.Context, req *pub.ChatParamsRequest) pub.ChatParams {
	dispatchChain(ctx, h, pub.HookChatParams, req, func(req *pub.ChatParamsRequest, resp *chatParamsResponse) bool {
		if resp.has {
			req.Params = resp.Params
		}
		return true
	}, nil)
	return req.Params
}

// ChatMessage runs the chat.message chain and returns the final message.
func (h *Host) ChatMessage(ctx context.Context, req *pub.ChatMessageRequest) pub.Message {
	dispatchChain(ctx, h, pub.HookChatMessage, req, func(req *pub.ChatMessageRequest, resp *chatMessageResponse) bool {
		if resp.has {
			req.Message = resp.Message
		}
		return true
	}, nil)
	return req.Message
}
