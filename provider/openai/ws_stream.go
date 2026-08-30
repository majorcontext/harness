package openai

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// wsFrame is one already-read websocket frame, buffered so the pool can
// look at the FIRST event before handing a stream to the caller (see
// wsPool.stream) without that event being lost once it does.
type wsFrame struct {
	name string
	data []byte
}

// wsFrameSource adapts a pooled *websocket.Conn to stream's frame-source
// contract (see stream.readEvent/Close in openai.go), so stream.handle —
// the Responses event-to-provider.Event mapper — runs UNCHANGED for a
// websocket-delivered response exactly as it does for an SSE-delivered one.
// Only how a (name, data) pair is obtained differs.
type wsFrameSource struct {
	conn        *websocket.Conn
	idleTimeout time.Duration
	buffered    *wsFrame

	// onTerminal fires exactly once, the first time a terminal event type
	// is observed (from the buffered first frame or a later read) — never
	// from Close, so a stream that is Closed after reaching Next() io.EOF
	// does not double-report. name is the event's wire type.
	onTerminal func(name string)
	// onBroken fires when the connection dies before a terminal event is
	// observed: a read error, or Close() called while the stream is still
	// mid-flight (context canceled, engine gave up on the turn). It never
	// fires after onTerminal has already fired for this source.
	onBroken func(err error)

	terminal bool
}

// next returns the next (name, data) pair, buffered first-frame included.
// It satisfies the same shape stream.readSSE does.
func (w *wsFrameSource) next() (string, []byte, error) {
	if w.buffered != nil {
		f := w.buffered
		w.buffered = nil
		w.observe(f.name, nil)
		return f.name, f.data, nil
	}
	ctx := context.Background()
	if w.idleTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.idleTimeout)
		defer cancel()
	}
	name, data, err := readResponsesFrame(ctx, w.conn)
	if err != nil {
		w.observe("", err)
		return "", nil, err
	}
	w.observe(name, nil)
	return name, data, nil
}

// observe records a successfully read event's terminality, or a read
// failure — each reported to the pool at most once per source.
func (w *wsFrameSource) observe(name string, err error) {
	if w.terminal {
		return
	}
	switch {
	case err != nil:
		w.terminal = true
		if w.onBroken != nil {
			w.onBroken(err)
		}
	case isWSTerminalEvent(name):
		w.terminal = true
		if w.onTerminal != nil {
			w.onTerminal(name)
		}
	}
}

// close implements stream.Close for a websocket-backed stream. clean is
// true when the stream's own decode loop already reached its terminal
// event (stream.done) — in that case onTerminal has already told the pool
// whether to keep or drop the connection, and close must not additionally
// terminate a connection the pool chose to keep pooled. clean is false for
// every other reason Close is called (context canceled mid-turn, a decode
// error stream.handle returned, the caller simply giving up) — the
// connection cannot be trusted for reuse, so it is torn down and reported
// exactly like a read failure.
func (w *wsFrameSource) close(clean bool) error {
	if clean {
		return nil
	}
	w.observe("", errStreamClosedEarly)
	return w.conn.Close(websocket.StatusNormalClosure, "")
}
