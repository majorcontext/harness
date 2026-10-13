package acp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// held is one session of the connection: its handle, its event pump, and the
// prompts in flight.
type held struct {
	id        string
	sess      *harness.Session
	cwd       string
	log       *slog.Logger
	send      func(method string, params any) error
	toolImage func(ctx context.Context, key string) (io.ReadCloser, error)

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
func newHeld(ctx context.Context, sess *harness.Session, cwd string, after uint64, log *slog.Logger, send func(string, any) error, toolImage func(context.Context, string) (io.ReadCloser, error)) *held {
	ctx, stop := context.WithCancel(ctx)
	h := &held{id: sess.ID(), sess: sess, cwd: cwd, log: log, send: send, toolImage: toolImage, stop: stop, stopped: make(chan struct{}),
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

// pumpState is what the pump has sent and still needs: the chunk text of each
// item, and the content of each tool call that waits for its result.
type pumpState struct {
	sent  map[chunkKey]string
	diffs map[string][]any
}

func (h *held) pump(ctx context.Context, after uint64) {
	defer close(h.stopped)
	defer h.finish()
	st := &pumpState{sent: map[chunkKey]string{}, diffs: map[string][]any{}}
	for e, err := range h.sess.Events(ctx, after) {
		if err != nil {
			if ctx.Err() == nil {
				h.log.Info("acp: event stream ended", "session", h.id, "error", err.Error())
			}
			return
		}
		if err := h.forward(ctx, e, st); err != nil {
			return
		}
		if !e.Ephemeral {
			h.advance(e.Seq)
		}
	}
}

func (h *held) forward(ctx context.Context, e protocol.Event, st *pumpState) error {
	sent := st.sent
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
		sent[chunkKey{f.ItemID, f.Type}] += f.Text
		return h.chunk(kind, f.ItemID, f.Text)
	case protocol.KindTurnEnded:
		clear(st.diffs)
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
		if !ok {
			return nil
		}
		switch done.Message.Role {
		case protocol.RoleAssistant:
			if err := h.assistantText(done, sent); err != nil {
				return err
			}
			return h.toolCall(done.Message, st)
		case protocol.RoleTool:
			return h.toolResult(ctx, done.Message, st)
		}
	}
	return nil
}

// assistantText sends the text and the reasoning of a completed assistant item
// that the deltas did not cover.
func (h *held) assistantText(done *protocol.ItemCompleted, sent map[chunkKey]string) error {
	for _, typ := range []string{protocol.LogPartReasoning, protocol.LogPartText} {
		var b strings.Builder
		for _, part := range done.Message.Parts {
			if part.Type == typ {
				b.WriteString(part.Text)
			}
		}
		key := chunkKey{done.ItemID, typ}
		full := b.String()
		text, resend := tailOf(sent[key], full)
		delete(sent, key)
		if text == "" {
			continue
		}
		id := done.ItemID
		if resend {
			h.log.Warn("acp: item text differs from the chunks sent; sending it whole", "session", h.id, "item", id)
			id += "_full"
		}
		if err := h.chunk(chunkUpdate(typ), id, text); err != nil {
			return err
		}
	}
	return nil
}

// tailOf returns the text to send at the end of an item whose chunks sent
// already went out. A full subscriber can drop a delta frame, so sent may not be
// a prefix of full. Then the client text cannot be repaired, because ACP v1 has
// no retraction, and tailOf returns the whole of full with resend set: the
// caller sends it as a new message.
func tailOf(sent, full string) (text string, resend bool) {
	if !utf8.ValidString(sent) || !strings.HasPrefix(full, sent) {
		return full, true
	}
	return full[len(sent):], false
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
