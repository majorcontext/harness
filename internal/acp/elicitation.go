package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/acp/rpc"
	"github.com/majorcontext/harness/protocol"
)

// ask is a question that the pump saw open and did not send to the client yet.
type ask struct {
	requestID string
	form      questionForm
}

// questionForm is the elicitation form of the question of a request. keys are the property names in question order, and questions the text that each key answers.
type questionForm struct {
	message   string
	schema    map[string]any
	keys      []string
	questions []string
	// others holds the key of the free-text property of each question in keys, or "" for a question that has none.
	others []string
	multi  []bool
}

// previewRunes bounds the preview that an option description carries.
const previewRunes = 200

type questionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview"`
}

type questionPayload struct {
	Questions []struct {
		Question    string           `json:"question"`
		Header      string           `json:"header"`
		MultiSelect bool             `json:"multiSelect"`
		Options     []questionOption `json:"options"`
	} `json:"questions"`
}

// formOf builds the form of a question request payload. It reports false for a payload with no question.
func formOf(payload json.RawMessage) (questionForm, bool) {
	var p questionPayload
	if json.Unmarshal(payload, &p) != nil || len(p.Questions) == 0 {
		return questionForm{}, false
	}
	var f questionForm
	props := map[string]any{}
	var required []string
	for i, q := range p.Questions {
		key := fmt.Sprintf("question_%d", i+1)
		title := q.Header
		if title == "" {
			title = q.Question
		}
		prop := map[string]any{"type": "string", "title": title, "description": q.Question}
		other := ""
		if len(q.Options) > 0 {
			prop = optionProperty(prop, q.MultiSelect, q.Options)
			other = key + "_other"
			props[other] = map[string]any{"type": "string", "title": "Other", "description": "Your own answer to: " + q.Question}
		} else {
			required = append(required, key)
		}
		props[key] = prop
		f.keys = append(f.keys, key)
		f.others = append(f.others, other)
		f.multi = append(f.multi, q.MultiSelect)
		f.questions = append(f.questions, q.Question)
	}
	f.message = strings.Join(f.questions, "\n")
	f.schema = map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		f.schema["required"] = required
	}
	return f, true
}

// optionProperty adds the options of a question to its property: a titled oneOf for one answer, and an array of the same titled options for more.
func optionProperty(prop map[string]any, multi bool, options []questionOption) map[string]any {
	titled := make([]any, len(options))
	for i, o := range options {
		option := map[string]any{"const": o.Label, "title": o.Label}
		if d := optionText(o, multi); d != "" {
			option["description"] = d
		}
		titled[i] = option
	}
	if multi {
		prop["type"], prop["minItems"] = "array", 1
		prop["items"] = map[string]any{"anyOf": titled}
		return prop
	}
	prop["oneOf"] = titled
	return prop
}

// optionText is the description of an option, followed by its preview when the question allows one answer. The preview is cut at previewRunes.
func optionText(o questionOption, multi bool) string {
	if multi || o.Preview == "" {
		return o.Description
	}
	p := []rune(o.Preview)
	if len(p) > previewRunes {
		p = append(p[:previewRunes], '…')
	}
	if o.Description == "" {
		return string(p)
	}
	return o.Description + "\n\n" + string(p)
}

// answer maps the text of each answered question to the answer text. It reports false when the client answered no question.
func (f questionForm) answer(content map[string]any) (json.RawMessage, bool) {
	answers := map[string]string{}
	for i, key := range f.keys {
		switch v := content[key].(type) {
		case nil:
		case string:
			answers[f.questions[i]] = v
		case []any:
			parts := make([]string, len(v))
			for j, item := range v {
				parts[j] = fmt.Sprint(item)
			}
			answers[f.questions[i]] = strings.Join(parts, ", ")
		default:
			answers[f.questions[i]] = fmt.Sprint(v)
		}
		if f.others[i] == "" {
			continue
		}
		if text, _ := content[f.others[i]].(string); strings.TrimSpace(text) != "" {
			if prev, ok := answers[f.questions[i]]; ok && prev != "" && f.multi[i] {
				text = prev + ", " + text
			}
			answers[f.questions[i]] = text
		}
	}
	if len(answers) == 0 {
		return nil, false
	}
	b, err := json.Marshal(answers)
	return b, err == nil
}

// opened records a question request of the turn that runs. The pump sends it to the client when the turn ends.
func (h *held) opened(e protocol.Event, st *pumpState) {
	if h.elicit == nil {
		return
	}
	p, err := e.Payload()
	if err != nil {
		return
	}
	req, ok := p.(*protocol.RequestOpened)
	if !ok || req.RequestKind != protocol.RequestQuestion {
		return
	}
	form, ok := formOf(req.Payload)
	if !ok {
		h.log.Warn("acp: question with no form", "session", h.id, "request", req.RequestID)
		return
	}
	st.asks = append(st.asks, ask{requestID: req.RequestID, form: form})
}

// resolved ends what the client still owes for a request that the log resolved: it cancels the elicitation that has no answer, and it fails the tool call of a dismissed question.
func (h *held) resolved(e protocol.Event, st *pumpState) error {
	p, err := e.Payload()
	if err != nil {
		return nil
	}
	res, ok := p.(*protocol.RequestResolved)
	if !ok {
		return nil
	}
	st.asks = removeAsk(st.asks, res.RequestID)
	h.mu.Lock()
	pending := h.elicits[res.RequestID]
	delete(h.elicits, res.RequestID)
	h.mu.Unlock()
	if pending != nil {
		pending.Cancel()
	}
	if res.Resolution != protocol.ResolutionDismissed {
		return nil
	}
	return h.send("session/update", map[string]any{"sessionId": h.id, "update": toolUpdateBody{Kind: "tool_call_update", ID: res.RequestID,
		Status: "failed", RawOutput: protocol.DismissalText(res.SourceLabel), Content: []any{textItem(protocol.DismissalText(res.SourceLabel))}}})
}

func removeAsk(asks []ask, id string) []ask {
	out := asks[:0]
	for _, a := range asks {
		if a.requestID != id {
			out = append(out, a)
		}
	}
	return out
}

// elicitAsks sends the elicitation of each question that the turn left open, when the turn ended awaiting_input. A question that a turn of another end left open gets none.
func (h *held) elicitAsks(ctx context.Context, e protocol.Event, st *pumpState) {
	asks := st.asks
	st.asks = nil
	p, err := e.Payload()
	if err != nil {
		return
	}
	if end, ok := p.(*protocol.TurnEnded); !ok || end.StopReason != protocol.StopAwaitingInput {
		return
	}
	for _, a := range asks {
		params := map[string]any{"mode": "form", "sessionId": h.id, "toolCallId": a.requestID, "message": a.form.message, "requestedSchema": a.form.schema}
		pending, err := h.elicit("elicitation/create", params)
		if err != nil {
			h.log.Info("acp: elicitation not sent", "session", h.id, "request", a.requestID, "error", err.Error())
			return
		}
		h.mu.Lock()
		h.elicits[a.requestID] = pending
		h.mu.Unlock()
		h.answers.Add(1)
		go h.answered(ctx, pending, a)
	}
}

// answered waits for the client to answer an elicitation, then resolves the request. The resolve outlives the pump, so an answer that arrived before a close is not lost. An error of the client, and the end of the connection, leave the request open.
func (h *held) answered(ctx context.Context, pending *rpc.Pending, a ask) {
	defer h.answers.Done()
	var res struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
	}
	err := pending.Wait(context.WithoutCancel(ctx), &res)
	h.mu.Lock()
	if h.elicits[a.requestID] == pending {
		delete(h.elicits, a.requestID)
	}
	h.mu.Unlock()
	if err != nil {
		if !errors.Is(err, rpc.ErrCancelled) && ctx.Err() == nil {
			h.log.Info("acp: elicitation failed; the question stays open", "session", h.id, "request", a.requestID, "error", err.Error())
		}
		return
	}
	var r protocol.Resolution
	switch res.Action {
	case "accept":
		answer, ok := a.form.answer(res.Content)
		r = protocol.Resolution{Answer: answer, Dismiss: !ok}
	case "decline", "cancel":
		r.Dismiss = true
	default:
		h.log.Info("acp: elicitation action unknown; the question stays open", "session", h.id, "request", a.requestID, "action", res.Action)
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interruptBudget)
	defer cancel()
	if _, err := h.sess.Resolve(rctx, a.requestID, r); err != nil && !errors.Is(err, harness.ErrRequestNotPending) {
		h.log.Warn("acp: resolve of an elicitation failed", "session", h.id, "request", a.requestID, "error", err.Error())
	}
}

// cancelElicitations cancels every elicitation that has no answer. The pump calls it when it ends.
func (h *held) cancelElicitations() {
	h.mu.Lock()
	pendings := h.elicits
	h.elicits = map[string]*rpc.Pending{}
	h.mu.Unlock()
	for _, p := range pendings {
		p.Cancel()
	}
}
