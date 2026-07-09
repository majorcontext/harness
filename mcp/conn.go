package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// transport is the abstraction both the stdio and Streamable HTTP
// transports implement. call sends a request and decodes the peer's
// result; notify sends a fire-and-forget notification.
type transport interface {
	call(ctx context.Context, method string, params, result any) error
	notify(ctx context.Context, method string, params any) error
	close() error
}

// notificationHandler observes a notification (or, for the stdio
// transport's duplex stream, an unsupported server-initiated request) that
// this client does not model. The default behavior is log-and-continue.
type notificationHandler func(method string, params json.RawMessage)

// handlerFunc serves one incoming request or notification arriving on a
// conn. The returned value is marshaled as the JSON-RPC result for
// requests (those with an ID) and ignored for notifications.
type handlerFunc func(ctx context.Context, method string, params json.RawMessage) (any, error)

// conn is a bidirectional, newline-delimited JSON-RPC 2.0 connection over a
// byte stream, used by the stdio transport (and, in tests, by the
// in-package fake stdio server on the other end of the same pipe) — the
// same conn/handlerFunc split plugin/protocol.go uses for its own hand-
// rolled JSON-RPC. Incoming requests are served on their own goroutine so a
// handler blocked on writing never stalls the read loop, and every
// outgoing call races its response against ctx.Done() and the connection
// closing.
type conn struct {
	wmu sync.Mutex // serializes writes
	rwc io.ReadWriteCloser
	r   *bufio.Reader

	handler handlerFunc
	nextID  atomic.Int64

	pmu     sync.Mutex
	pending map[string]chan message

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

func newConn(rwc io.ReadWriteCloser, handler handlerFunc) *conn {
	if handler == nil {
		handler = func(_ context.Context, method string, _ json.RawMessage) (any, error) {
			return nil, &RPCError{Code: codeMethodNotFound, Message: fmt.Sprintf("unsupported method %q", method)}
		}
	}
	return &conn{
		rwc:     rwc,
		r:       bufio.NewReader(rwc),
		handler: handler,
		pending: make(map[string]chan message),
		closed:  make(chan struct{}),
	}
}

// run reads and dispatches messages until the stream ends. Every message is
// exactly one line (newline-delimited JSON-RPC per the stdio transport
// spec); messages MUST NOT contain embedded newlines.
func (c *conn) run() error {
	for {
		line, err := c.r.ReadBytes('\n')
		if len(line) > 1 {
			var msg message
			if uerr := json.Unmarshal(line, &msg); uerr == nil {
				c.dispatch(msg)
			}
			// A malformed line from a peer that isn't speaking JSON-RPC is
			// dropped; there is no request ID to reply to.
		}
		if err != nil {
			c.fail(err)
			return err
		}
	}
}

func (c *conn) dispatch(msg message) {
	if msg.isResponse() {
		c.pmu.Lock()
		ch, ok := c.pending[idToken(msg.ID)]
		delete(c.pending, idToken(msg.ID))
		c.pmu.Unlock()
		if ok {
			ch <- msg
		}
		return
	}
	if msg.isRequestOrNotification() {
		go c.serveRequest(msg)
	}
}

func (c *conn) serveRequest(msg message) {
	result, err := c.handler(context.Background(), msg.Method, msg.Params)
	if msg.isNotification() {
		return // notifications get no response, whatever the handler returned
	}
	resp := message{ID: msg.ID}
	if err != nil {
		var rerr *RPCError
		if !errors.As(err, &rerr) {
			rerr = &RPCError{Code: codeInternalError, Message: err.Error()}
		}
		resp.Error = rerr
	} else {
		raw, merr := json.Marshal(result)
		if merr != nil {
			resp.Error = &RPCError{Code: codeInternalError, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	}
	// A write failure means the stream is going down; the read loop will
	// surface it.
	_ = c.write(resp)
}

func (c *conn) call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	idJSON, err := json.Marshal(id)
	if err != nil {
		return err
	}
	key := idToken(idJSON)
	ch := make(chan message, 1)
	c.pmu.Lock()
	c.pending[key] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, key)
		c.pmu.Unlock()
	}()

	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	if err := c.write(message{ID: idJSON, Method: method, Params: raw}); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		// Best-effort: tell the peer we're no longer waiting, per the
		// cancellation utility. The initialize request MUST NOT be
		// cancelled per spec; callers are responsible for not cancelling
		// it (a race is harmless here since it's advisory).
		_ = c.notify(context.Background(), notificationCancelled, cancelledParams{
			RequestID: idJSON,
			Reason:    ctx.Err().Error(),
		})
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("mcp: connection closed: %w", c.closeErr)
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

func (c *conn) notify(_ context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(message{Method: method, Params: raw})
}

func (c *conn) write(msg message) error {
	msg.JSONRPC = "2.0"
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rwc.Write(raw)
	return err
}

func (c *conn) fail(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		close(c.closed)
	})
}

func (c *conn) close() error {
	c.fail(io.ErrClosedPipe)
	return c.rwc.Close()
}
