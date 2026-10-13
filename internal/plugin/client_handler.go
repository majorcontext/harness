package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	pub "github.com/majorcontext/harness/plugin"

	"github.com/majorcontext/harness/internal/plugin/wire"
)

func (h *Host) clientHandler(name string) wire.Handler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if h.opts.Client == nil {
			return nil, &wire.Error{Code: wire.CodeMethodNotFound, Message: "client API not available"}
		}
		switch method {
		case wire.MethodSessionMessages:
			var req pub.SessionMessagesRequest
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, err
			}
			return h.opts.Client.SessionMessages(ctx, &req)
		case wire.MethodMCPCall:
			var req pub.MCPCallRequest
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, err
			}
			return h.opts.Client.MCPCall(ctx, &req)
		case wire.MethodGenerate:
			var req pub.GenerateRequest
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, err
			}
			resp, err := h.opts.Client.Generate(ctx, &req)
			if err != nil {
				return nil, err
			}
			args := []any{"plugin", name, "model", resp.Message.Model}
			if u := resp.Usage; u != nil {
				args = append(args, "input_tokens", u.InputTokens, "output_tokens", u.OutputTokens)
			}
			h.opts.log().Info("plugin: generate", args...)
			return resp, nil
		default:
			return nil, &wire.Error{Code: wire.CodeMethodNotFound, Message: fmt.Sprintf("unknown method %q", method)}
		}
	}
}

func (o Options) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}
