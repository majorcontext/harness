package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/acp/rpc"
	"github.com/majorcontext/harness/protocol"
)

// interruptBudget bounds the interrupt of a turn whose prompt the peer
// cancelled or whose connection ended.
const interruptBudget = 5 * time.Second

type promptParams struct {
	SessionID string            `json:"sessionId"`
	Prompt    []json.RawMessage `json:"prompt"`
}

// promptParts maps the content blocks of a prompt to input parts. The agent
// advertises no prompt capability, so it takes the baseline: text and
// resource_link.
func promptParts(blocks []json.RawMessage) ([]protocol.Part, error) {
	var parts []protocol.Part
	for i, raw := range blocks {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
			URI  string `json:"uri"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, invalidParams(fmt.Sprintf("prompt[%d] is not a content block", i))
		}
		switch b.Type {
		case "text":
			parts = append(parts, protocol.Part{Type: protocol.PartText, Text: b.Text})
		case "resource_link":
			parts = append(parts, protocol.Part{Type: protocol.PartText, Text: b.Name + " (" + b.URI + ")"})
		default:
			return nil, invalidParams(fmt.Sprintf("prompt[%d] has the unsupported type %q", i, b.Type))
		}
	}
	if len(parts) == 0 {
		return nil, invalidParams("prompt has no content block")
	}
	return parts, nil
}

func (a *agent) prompt(ctx context.Context, params json.RawMessage) (any, error) {
	var p promptParams
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" || p.Prompt == nil {
		return nil, invalidParams("sessionId and prompt are required")
	}
	h, err := a.held(p.SessionID)
	if err != nil {
		return nil, err
	}
	parts, err := promptParts(p.Prompt)
	if err != nil {
		return nil, err
	}
	pr := h.begin()
	defer h.end(pr)
	if h.cancelled(pr) {
		return map[string]string{"stopReason": "cancelled"}, nil
	}
	turn, err := h.sess.Prompt(ctx, protocol.Input{ID: newULID(), Parts: parts, Delivery: protocol.DeliveryQueue})
	if err != nil {
		return nil, rpcError(err)
	}
	if h.admitted(pr, turn) {
		go h.cancelTurn(context.WithoutCancel(ctx), turn)
	}
	res, err := turn.Wait(ctx)
	if ctx.Err() != nil {
		h.interrupt(ctx, turn)
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, rpcError(err)
	}
	h.flush(ctx, h.sess.Snapshot().HeadSeq)
	return promptResult(res)
}

// interrupt ends the turn of a prompt whose request ended before the turn did.
func (h *held) interrupt(ctx context.Context, t *harness.Turn) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), interruptBudget)
	defer cancel()
	h.cancelTurn(bg, t)
}

// promptResult maps the outcome of a turn to the answer of session/prompt.
func promptResult(res harness.TurnResult) (any, error) {
	if res.Withdrawn {
		return map[string]string{"stopReason": "cancelled"}, nil
	}
	switch res.StopReason {
	case protocol.StopCompleted, protocol.StopAwaitingInput:
		return map[string]string{"stopReason": "end_turn"}, nil
	case protocol.StopInterrupted:
		return map[string]string{"stopReason": "cancelled"}, nil
	}
	msg := res.Error
	if msg == "" {
		msg = "Turn failed"
	}
	return nil, &rpc.Error{Code: rpc.CodeInternal, Message: msg, Data: map[string]any{
		"code": string(protocol.CodeInternal), "error_class": string(res.ErrorClass)}}
}
