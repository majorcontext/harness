package acp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/majorcontext/harness"
)

// closeSession cancels the work of a session as session/cancel does, waits for
// the prompts to answer and for the pump to send their updates, then stops the
// pump and releases the session. The log stays.
func (a *agent) closeSession(ctx context.Context, params json.RawMessage) (any, error) {
	return a.ordered(ctx, params, func() (any, error) { return a.closeNow(ctx, params) })
}

func (a *agent) closeNow(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return nil, invalidParams("sessionId is required")
	}
	a.mu.Lock()
	h, ok := a.sessions[p.SessionID]
	delete(a.sessions, p.SessionID)
	a.mu.Unlock()
	if !ok {
		return nil, rpcError(harness.ErrSessionNotFound)
	}
	h.cancel(a.base)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
	defer cancel()
	h.idle(bg)
	h.flush(bg, h.sess.Snapshot().HeadSeq)
	h.close()
	if err := h.sess.Release(bg); err != nil && !errors.Is(err, harness.ErrSessionNotOwned) {
		return nil, rpcError(err)
	}
	return struct{}{}, nil
}
