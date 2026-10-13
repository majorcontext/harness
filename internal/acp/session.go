package acp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// held is one session of the connection: its handle, its event pump, and the
// prompts in flight.
type held struct {
	id   string
	sess *harness.Session
	log  *slog.Logger
	send func(method string, params any) error

	stop    context.CancelFunc
	stopped chan struct{}

	mu      sync.Mutex
	done    uint64
	ended   bool
	wake    chan struct{}
	prompts map[*prompt]struct{}
}

// prompt is one session/prompt in flight. cancelled records a session/cancel
// that arrived before the turn was admitted.
type prompt struct {
	turn      *harness.Turn
	cancelled bool
}

// newHeld starts the event pump of sess at after, the head of the log when the
// connection took the session. The pump is the only reader of the events.
func newHeld(ctx context.Context, sess *harness.Session, after uint64, log *slog.Logger, send func(string, any) error) *held {
	ctx, stop := context.WithCancel(ctx)
	h := &held{id: sess.ID(), sess: sess, log: log, send: send, stop: stop, stopped: make(chan struct{}),
		wake: make(chan struct{}), prompts: map[*prompt]struct{}{}}
	go h.pump(ctx, after)
	return h
}

// close stops the pump and waits for it.
func (h *held) close() {
	h.stop()
	<-h.stopped
}

type chunkKey struct{ item, kind string }

func (h *held) pump(ctx context.Context, after uint64) {
	defer close(h.stopped)
	defer h.finish()
	sent := map[chunkKey]int{}
	for e, err := range h.sess.Events(ctx, after) {
		if err != nil {
			if ctx.Err() == nil {
				h.log.Info("acp: event stream ended", "session", h.id, "error", err.Error())
			}
			return
		}
		if err := h.forward(e, sent); err != nil {
			return
		}
		if !e.Ephemeral {
			h.advance(e.Seq)
		}
	}
}

func (h *held) forward(e protocol.Event, sent map[chunkKey]int) error {
	switch e.Kind {
	case protocol.KindItemDelta:
		var f protocol.ItemFrame
		if json.Unmarshal(e.Data, &f) != nil || f.Text == "" {
			return nil
		}
		kind := chunkUpdate(f.Type)
		if kind == "" {
			return nil
		}
		sent[chunkKey{f.ItemID, f.Type}] += len(f.Text)
		return h.chunk(kind, f.ItemID, f.Text)
	case protocol.KindItemDiscarded:
		var f protocol.ItemFrame
		if json.Unmarshal(e.Data, &f) == nil {
			delete(sent, chunkKey{f.ItemID, protocol.LogPartText})
			delete(sent, chunkKey{f.ItemID, protocol.LogPartReasoning})
		}
	case protocol.KindItemCompleted:
		p, err := e.Payload()
		if err != nil {
			return nil
		}
		done, ok := p.(*protocol.ItemCompleted)
		if !ok || done.Message.Role != protocol.RoleAssistant {
			return nil
		}
		for _, typ := range []string{protocol.LogPartReasoning, protocol.LogPartText} {
			var b strings.Builder
			for _, part := range done.Message.Parts {
				if part.Type == typ {
					b.WriteString(part.Text)
				}
			}
			key := chunkKey{done.ItemID, typ}
			full, n := b.String(), sent[key]
			delete(sent, key)
			if n < len(full) {
				if err := h.chunk(chunkUpdate(typ), done.ItemID, full[n:]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func chunkUpdate(partType string) string {
	switch partType {
	case protocol.LogPartText:
		return "agent_message_chunk"
	case protocol.LogPartReasoning:
		return "agent_thought_chunk"
	}
	return ""
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type chunkUpdateBody struct {
	Kind      string      `json:"sessionUpdate"`
	Content   textContent `json:"content"`
	MessageID string      `json:"messageId"`
}

func (h *held) chunk(kind, itemID, text string) error {
	id := "msg_" + itemID
	if kind == "agent_thought_chunk" {
		id = "thought_" + itemID
	}
	return h.send("session/update", map[string]any{
		"sessionId": h.id,
		"update":    chunkUpdateBody{Kind: kind, Content: textContent{Type: "text", Text: text}, MessageID: id},
	})
}

// advance records that the pump sent every update through seq.
func (h *held) advance(seq uint64) {
	h.mu.Lock()
	h.done = seq
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

func (h *held) finish() {
	h.mu.Lock()
	h.ended = true
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

// flush waits until the pump has sent every update through seq, or has ended.
func (h *held) flush(ctx context.Context, seq uint64) {
	for {
		h.mu.Lock()
		ready, wake := h.done >= seq || h.ended, h.wake
		h.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return
		}
	}
}

// begin registers a prompt before it is admitted.
func (h *held) begin() *prompt {
	p := &prompt{}
	h.mu.Lock()
	h.prompts[p] = struct{}{}
	h.mu.Unlock()
	return p
}

// admitted attaches the turn to p and reports whether a cancel already
// arrived.
func (h *held) admitted(p *prompt, t *harness.Turn) (cancelled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p.turn = t
	return p.cancelled
}

func (h *held) end(p *prompt) {
	h.mu.Lock()
	delete(h.prompts, p)
	h.mu.Unlock()
}

// cancel marks every prompt in flight as cancelled and cancels the turns that
// are admitted. It does not block.
func (h *held) cancel(ctx context.Context) {
	h.mu.Lock()
	var turns []*harness.Turn
	for p := range h.prompts {
		p.cancelled = true
		if p.turn != nil {
			turns = append(turns, p.turn)
		}
	}
	h.mu.Unlock()
	for _, t := range turns {
		go h.cancelTurn(ctx, t)
	}
}

func (h *held) cancelTurn(ctx context.Context, t *harness.Turn) {
	if err := t.Cancel(ctx); err != nil && ctx.Err() == nil {
		h.log.Warn("acp: cancel failed", "session", h.id, "error", err.Error())
	}
}

func (h *held) cancelled(p *prompt) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return p.cancelled
}
