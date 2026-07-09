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
	"time"
)

// cancelledNotifyTimeout bounds the best-effort notifications/cancelled
// write sent when a call's context is done. It is deliberately short and
// detached from the caller's ctx (which is already done) so a peer that
// stopped reading can't hang this cleanup goroutine indefinitely.
const cancelledNotifyTimeout = 1 * time.Second

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
	// wmu is a 1-buffered channel semaphore serializing writes, in place
	// of a sync.Mutex: acquiring it selects on ctx.Done() (see
	// acquireWmu), so a write bounded by a deadline (e.g. the
	// cancelled-notify cleanup goroutine below) can never be stuck behind
	// another writer for longer than its own deadline. A plain
	// sync.Mutex has no such escape hatch — Lock cannot be given up on.
	wmu chan struct{}
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
		wmu:     make(chan struct{}, 1),
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
	// surface it. Responses to served requests carry no deadline of their
	// own (context.Background()): unlike the cancelled-notify cleanup
	// below, there is no caller-side timeout to protect here.
	_ = c.write(context.Background(), resp)
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
	if err := c.write(ctx, message{ID: idJSON, Method: method, Params: raw}); err != nil {
		// A write aborted because ctx ran out (of the wmu acquisition or
		// the write itself) is indistinguishable from any other write
		// failure to the caller except that ctx.Err() is the more
		// meaningful error to surface.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}

	select {
	case <-ctx.Done():
		// Best-effort: tell the peer we're no longer waiting, per the
		// cancellation utility. The initialize request MUST NOT be
		// cancelled per spec; callers are responsible for not cancelling
		// it (a race is harmless here since it's advisory).
		//
		// This must never make the already-cancelled/timed-out call wait
		// any longer: it fires in its own goroutine, bounded by a short
		// deadline. That deadline is enforced where the blocking actually
		// happens — write() acquires wmu (and, on transports that support
		// it, sets a write deadline) via notifyCtx, so a peer that
		// stopped reading can wedge this cleanup goroutine for at most
		// cancelledNotifyTimeout, never longer, and it can never hold wmu
		// past its own deadline to defeat a later call's timeout.
		reason := ctx.Err().Error()
		go func() {
			notifyCtx, cancel := context.WithTimeout(context.Background(), cancelledNotifyTimeout)
			defer cancel()
			_ = c.notify(notifyCtx, notificationCancelled, cancelledParams{
				RequestID: idJSON,
				Reason:    reason,
			})
		}()
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

func (c *conn) notify(ctx context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(ctx, message{Method: method, Params: raw})
}

// writeDeadliner is implemented by transports (e.g. net.Conn) that can
// enforce a write deadline directly; stdio pipes generally don't, so for
// those the ctx-aware wmu acquisition in acquireWmu is what bounds the
// wait.
type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

// write serializes msg and sends it, bounded by ctx: acquiring wmu selects
// on ctx.Done() (acquireWmu), and, when the underlying transport supports
// it, a write deadline derived from ctx is set before the write so the
// write syscall itself can't outlast ctx either. Either way, on ctx
// expiry write returns without leaving wmu held for a stuck caller (e.g.
// the best-effort cancelled-notify cleanup) to wedge every later write.
func (c *conn) write(ctx context.Context, msg message) error {
	msg.JSONRPC = "2.0"
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if err := c.acquireWmu(ctx); err != nil {
		return err
	}
	defer c.releaseWmu()

	if dl, ok := ctx.Deadline(); ok {
		if wd, ok := c.rwc.(writeDeadliner); ok {
			_ = wd.SetWriteDeadline(dl)
			defer wd.SetWriteDeadline(time.Time{}) //nolint:errcheck // best-effort clear; write has already returned
		}
	}
	_, err = c.rwc.Write(raw)
	return err
}

// acquireWmu acquires the write semaphore, giving up if ctx is done or the
// connection closes first. This is what makes writes bounded by a deadline
// (like the cancelled-notify cleanup) unable to get stuck behind another
// writer indefinitely.
func (c *conn) acquireWmu(ctx context.Context) error {
	select {
	case c.wmu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("mcp: connection closed: %w", c.closeErr)
	}
}

func (c *conn) releaseWmu() {
	<-c.wmu
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
