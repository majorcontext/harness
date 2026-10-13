package acp

import (
	"errors"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// lastSent is the state that the pump has sent in config_option_update and
// usage_update. The pump sends an update only when the session state differs.
type lastSent struct {
	model, effort string
	usage         usageState
}

type usageState struct {
	used, size int64
	cost       float64
	hasCost    bool
}

func usageOf(s protocol.Session) usageState {
	u := usageState{used: s.Context.Tokens, size: s.Context.Window}
	if s.SubscriptionUsage != nil && s.SubscriptionUsage.SessionCostUSD != nil {
		u.cost, u.hasCost = *s.SubscriptionUsage.SessionCostUSD, true
	}
	return u
}

func seedSent(s protocol.Session) lastSent {
	return lastSent{model: s.Model, effort: s.Effort, usage: usageOf(s)}
}

// update sends one session/update notification.
func (h *held) update(body any) error {
	return h.send("session/update", map[string]any{"sessionId": h.id, "update": body})
}

// configUpdate sends the complete options when the model or the effort of the
// session differs from what the pump sent. It reads the session, not the event,
// so a settings change later in the log is already in the update.
func (h *held) configUpdate(st *pumpState) error {
	s := h.sess.Snapshot()
	if s.Model == st.last.model && s.Effort == st.last.effort {
		return nil
	}
	st.last.model, st.last.effort = s.Model, s.Effort
	return h.update(map[string]any{"sessionUpdate": "config_option_update", "configOptions": configOptions(h.models, s)})
}

// usageUpdate sends the context size and the cost when they differ from what
// the pump sent. It sends none while the window of the model is not known.
func (h *held) usageUpdate(st *pumpState) error {
	u := usageOf(h.sess.Snapshot())
	if u == st.last.usage || u.size == 0 {
		return nil
	}
	st.last.usage = u
	body := map[string]any{"sessionUpdate": "usage_update", "used": u.used, "size": u.size}
	if u.hasCost {
		body["cost"] = map[string]any{"amount": u.cost, "currency": "USD"}
	}
	return h.update(body)
}

// sessionInfo sends the time of the last activity of the session.
func (h *held) sessionInfo() error {
	return h.update(map[string]any{"sessionUpdate": "session_info_update", "updatedAt": h.sess.Snapshot().UpdatedAt.UTC().Format(time.RFC3339)})
}

// commandStatus sends the status text of a typed command that has ended as a message.
func (h *held) commandStatus(e protocol.Event) error {
	p, err := e.Payload()
	if err != nil {
		return nil
	}
	rec, ok := p.(*protocol.CommandRecorded)
	if !ok || rec.Status == protocol.CommandAccepted || rec.Text == "" {
		return nil
	}
	return h.chunk("agent_message_chunk", "cmd_"+rec.InputID, rec.Text)
}

// lostOwnership tells a client that takes notices that another owner took the session.
func (h *held) lostOwnership(err error) {
	if !h.notices || !errors.Is(err, harness.ErrOwnershipLost) {
		return
	}
	_ = h.update(map[string]any{"sessionUpdate": "notice", "severity": "warning", "title": "Session taken by another owner",
		"description": "Another process opened this session. This connection can no longer run it."})
}
