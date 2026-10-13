package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
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
			Type string  `json:"type"`
			Text *string `json:"text"`
			Name *string `json:"name"`
			URI  *string `json:"uri"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, invalidParams(fmt.Sprintf("prompt[%d] is not a content block", i))
		}
		switch b.Type {
		case "text":
			if b.Text == nil {
				return nil, invalidParams(fmt.Sprintf("prompt[%d] has no text", i))
			}
			parts = append(parts, protocol.Part{Type: protocol.PartText, Text: *b.Text})
		case "resource_link":
			if b.Name == nil || b.URI == nil {
				return nil, invalidParams(fmt.Sprintf("prompt[%d] needs name and uri", i))
			}
			parts = append(parts, protocol.Part{Type: protocol.PartText, Text: *b.Name + " (" + *b.URI + ")"})
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
	pr, _ := ctx.Value(promptKey{}).(*prompt)
	if pr == nil {
		pr = h.begin()
	}
	defer h.end(pr)
	if h.cancelled(pr) {
		return map[string]string{"stopReason": "cancelled"}, nil
	}
	in := protocol.Input{ID: newULID(), Parts: parts, Delivery: protocol.DeliveryQueue}
	h.mu.Lock()
	pr.input = in.ID
	h.mu.Unlock()
	if isTyped(p.Prompt, parts) {
		in.Source = protocol.SourceTyped
	}
	turn, err := h.sess.Prompt(ctx, in)
	if err != nil {
		return nil, h.failure(ctx, err)
	}
	if h.admitted(pr, turn) {
		go h.cancelTurn(context.WithoutCancel(ctx), turn)
	}
	res, err := turn.Wait(ctx)
	if ctx.Err() != nil {
		h.release(pr)
		h.interrupt(ctx, turn)
		return nil, ctx.Err()
	}
	if err = waitFailure(err, h.cancelled(pr)); err != nil {
		h.release(pr)
		return nil, h.failure(ctx, err)
	}
	if res.TurnID == "" {
		h.flush(ctx, h.sess.Snapshot().HeadSeq)
	} else {
		h.reached(ctx, pr)
	}
	v, err := answer(ctx, res, h.cancelled(pr))
	if ctx.Err() != nil {
		return nil, err
	}
	return h.handOff(pr, v, err)
}

// isTyped says that a prompt is a typed line: exactly one text block whose text
// starts with a slash. The runtime then runs a slash command.
func isTyped(blocks []json.RawMessage, parts []protocol.Part) bool {
	if len(blocks) != 1 || len(parts) != 1 || !strings.HasPrefix(parts[0].Text, "/") {
		return false
	}
	var b struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(blocks[0], &b) == nil && b.Type == "text"
}

// failure maps the error of a prompt. When the connection does not own the
// session, it waits for the pump to end, so that a notice reaches the client before
// the error does.
func (h *held) failure(ctx context.Context, err error) error {
	if errors.Is(err, harness.ErrSessionNotOwned) {
		bg, cancel := context.WithTimeout(ctx, interruptBudget)
		defer cancel()
		h.flush(bg, math.MaxUint64)
	}
	return rpcError(err)
}

// interrupt ends the turn of a prompt whose request ended before the turn did.
func (h *held) interrupt(ctx context.Context, t *harness.Turn) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), interruptBudget)
	defer cancel()
	h.cancelTurn(bg, t)
	h.flush(bg, h.sess.Snapshot().HeadSeq)
}

// waitFailure returns the error of Turn.Wait to report. After a session/cancel
// the agent must answer cancelled, so any failure except lost ownership
// becomes nil and promptResult answers cancelled.
func waitFailure(err error, cancelled bool) error {
	if err != nil && cancelled && !errors.Is(err, harness.ErrSessionNotOwned) {
		return nil
	}
	return err
}

// answer returns the result of a prompt, or the error of ctx when the peer
// cancelled the request while the updates flushed, so the rpc layer answers -32800.
func answer(ctx context.Context, res harness.TurnResult, cancelled bool) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return promptResult(res, cancelled)
}

// promptResult maps the outcome of a turn to the answer of session/prompt.
func promptResult(res harness.TurnResult, cancelled bool) (any, error) {
	if res.Withdrawn || (cancelled && (res.StopReason == protocol.StopFailed || res.StopReason == "")) {
		return map[string]string{"stopReason": "cancelled"}, nil
	}
	switch {
	case res.Command == protocol.CommandInterrupted:
		return map[string]string{"stopReason": "cancelled"}, nil
	case res.Command != "":
		return map[string]string{"stopReason": "end_turn"}, nil
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

type promptKey struct{}

// Admit runs in the reader goroutine of the connection, in wire order, before
// the handler of a request starts. It registers a session/prompt, so a
// session/cancel that follows on the wire finds the prompt.
func (a *agent) Admit(ctx context.Context, method string, params json.RawMessage) context.Context {
	switch method {
	case "session/load", "session/resume", "session/close":
		if id := sessionIDOf(params); id != "" {
			return context.WithValue(ctx, ticketKey{}, a.reserve(id))
		}
		return ctx
	case "session/prompt":
	default:
		return ctx
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ctx
	}
	h, err := a.held(p.SessionID)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, promptKey{}, h.begin())
}
