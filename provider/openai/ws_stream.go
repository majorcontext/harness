package openai

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// wsFrame is a buffered WebSocket event.
type wsFrame struct {
	name string
	data []byte
}

// wsFrameSource adapts a pooled WebSocket connection to a stream frame source.
type wsFrameSource struct {
	ctx         context.Context
	conn        *websocket.Conn
	idleTimeout time.Duration
	buffered    *wsFrame

	// onTerminal reports the first terminal event.
	onTerminal func(name string, data []byte, first bool)
	// onBroken reports a read error before a terminal event.
	onBroken func(err error)

	terminal   bool
	framesRead int
}

// next returns the next event, including a buffered first event.
func (w *wsFrameSource) next() (string, []byte, error) {
	if w.buffered != nil {
		f := w.buffered
		w.buffered = nil
		w.observe(f.name, f.data, nil)
		return f.name, f.data, nil
	}
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if w.idleTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.idleTimeout)
		defer cancel()
	}
	name, data, err := readResponsesFrame(ctx, w.conn)
	if err != nil {
		w.observe("", nil, err)
		return "", nil, err
	}
	w.observe(name, data, nil)
	return name, data, nil
}

// observe reports the first terminal event or read failure.
func (w *wsFrameSource) observe(name string, data []byte, err error) {
	if w.terminal {
		return
	}
	if err == nil {
		w.framesRead++
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
			w.onTerminal(name, data, w.framesRead == 1)
		}
	}
}

// close preserves a connection after a clean terminal event.
func (w *wsFrameSource) close(clean bool) error {
	if clean {
		return nil
	}
	w.observe("", nil, errStreamClosedEarly)
	return w.conn.Close(websocket.StatusNormalClosure, "")
}
