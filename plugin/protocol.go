// Package plugin implements the harness plugin protocol and the SDK for
// writing plugins.
//
// Plugins are separate processes (any language; this package is the Go SDK)
// speaking JSON-RPC 2.0 over stdio, one message per line (NDJSON). The
// channel is bidirectional: the harness sends hook dispatches and tool
// executions to the plugin, and the plugin sends client API calls
// (client/session.messages, client/mcp.call, client/generate) back to the
// harness — including while a hook is in flight.
//
// See PROTOCOL.md for the versioned wire specification.
package plugin

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

// ProtocolVersion is the version of the hook protocol implemented by this
// package. The harness rejects plugins whose manifest declares a different
// version.
const ProtocolVersion = 1

// Method names. Hooks are dispatched as "hook/" + the Hook name.
const (
	methodInitialize  = "initialize"
	methodShutdown    = "shutdown"
	methodToolExecute = "tool/execute"
	hookMethodPrefix  = "hook/"

	// Plugin → harness client API.
	methodSessionMessages = "client/session.messages"
	methodMCPCall         = "client/mcp.call"
	methodGenerate        = "client/generate"
)

// JSON-RPC 2.0 error codes.
const (
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("plugin: rpc error %d: %s", e.Code, e.Message)
}

// rpcMessage is a JSON-RPC 2.0 request, notification, or response. IDs are
// always numbers in this protocol.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// handlerFunc serves one incoming request or notification. The returned value
// is marshaled as the result for requests and ignored for notifications.
type handlerFunc func(ctx context.Context, method string, params json.RawMessage) (any, error)

// conn is a bidirectional JSON-RPC 2.0 connection over a byte stream.
// notifyQueueSize bounds the per-connection backlog of incoming droppable
// notifications (hook/event) awaiting serial dispatch. Sized well above any
// realistic burst, but a full queue never blocks the read loop: see
// conn.dispatchNotification.
const notifyQueueSize = 1024

type conn struct {
	wmu sync.Mutex // serializes writes
	rwc io.ReadWriteCloser
	r   *bufio.Reader

	handler handlerFunc
	nextID  atomic.Int64

	pmu     sync.Mutex
	pending map[int64]chan rpcMessage

	// notifyCh feeds the dedicated notification-dispatch goroutine (see
	// runNotifications): notifications are handled one at a time, in
	// receipt order, so that e.g. two hook/event batches can never be
	// processed by racing handler goroutines and observed out of order by
	// the peer. Requests still get their own goroutine each (via
	// serveRequest) since a handler may need to make an outgoing call
	// before replying, which must not stall this queue.
	//
	// The read loop (conn.run, via dispatch) never blocks trying to feed
	// this channel: see dispatchNotification. Only droppable notifications
	// (hook/event) are ever queued here; shutdown is handled inline and
	// never touches notifyCh.
	notifyCh chan rpcMessage

	// notifyDropped counts notifications dropped by dispatchNotification
	// because notifyCh was saturated when they arrived.
	notifyDropped atomic.Uint64

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

func newConn(rwc io.ReadWriteCloser, handler handlerFunc) *conn {
	c := &conn{
		rwc:      rwc,
		r:        bufio.NewReader(rwc),
		handler:  handler,
		pending:  make(map[int64]chan rpcMessage),
		notifyCh: make(chan rpcMessage, notifyQueueSize),
		closed:   make(chan struct{}),
	}
	go c.runNotifications()
	return c
}

// runNotifications drains notifyCh in order, one at a time, for the life of
// the connection. It exits once the connection is closed.
func (c *conn) runNotifications() {
	for {
		select {
		case msg := <-c.notifyCh:
			c.serveRequest(msg)
		case <-c.closed:
			return
		}
	}
}

// run reads and dispatches messages until the stream ends. Incoming requests
// are served on their own goroutines so that a handler blocked on an outgoing
// call (e.g. a hook making a client API request) never stalls the read loop.
// Notifications are handed to the dedicated notification goroutine instead,
// preserving their receipt order (see runNotifications), except that this
// handoff never blocks: see dispatchNotification for how a saturated queue
// (or an undroppable notification) is handled without stalling this loop.
func (c *conn) run() error {
	for {
		line, err := c.r.ReadBytes('\n')
		if len(line) > 1 {
			var msg rpcMessage
			if uerr := json.Unmarshal(line, &msg); uerr == nil {
				c.dispatch(msg)
			}
			// Malformed lines are dropped; there is nothing useful to
			// return to a peer that isn't speaking the protocol.
		}
		if err != nil {
			c.fail(err)
			return err
		}
	}
}

func (c *conn) dispatch(msg rpcMessage) {
	if msg.Method != "" {
		if msg.ID == nil {
			c.dispatchNotification(msg)
			return
		}
		go c.serveRequest(msg)
		return
	}
	if msg.ID == nil {
		return
	}
	c.pmu.Lock()
	ch, ok := c.pending[*msg.ID]
	delete(c.pending, *msg.ID)
	c.pmu.Unlock()
	if ok {
		ch <- msg
	}
}

// dispatchNotification routes an incoming notification (a message with no
// ID) without ever blocking the read loop that calls it (conn.run, via
// dispatch). That read loop also carries RPC responses back to pending
// calls, so stalling it on notification delivery would wedge the whole
// connection, not just notifications — see PROTOCOL.md's event drop
// semantics, which this mirrors at the transport layer.
//
// Every notification this protocol defines is either:
//   - shutdown: not droppable — losing it would make conn.run's caller
//     (sdk.go's serve) mistake a routine harness-initiated stop for a read
//     error. Handling is a flag set plus closing the connection (see
//     server.handle), so it is cheap enough to run inline, right here on
//     the read-loop goroutine, and it bypasses notifyCh entirely: it can
//     never be starved by a backlog of anything else.
//   - hook/event: explicitly documented fire-and-forget/droppable
//     (PROTOCOL.md, "Chaining semantics" / event). This is the only other
//     notification method in the protocol (see the harness → plugin method
//     table), and any future droppable notification method belongs here
//     too. These queue on notifyCh for serial, in-order dispatch, but the
//     send is non-blocking: on a saturated queue (a pathological backlog
//     from a slow or wedged handler) the notification is dropped and
//     counted via notifyDropped instead of stalling the read loop.
func (c *conn) dispatchNotification(msg rpcMessage) {
	if msg.Method == methodShutdown {
		c.serveRequest(msg)
		return
	}
	select {
	case c.notifyCh <- msg:
	default:
		c.notifyDropped.Add(1)
	}
}

func (c *conn) serveRequest(msg rpcMessage) {
	result, err := c.handler(context.Background(), msg.Method, msg.Params)
	if msg.ID == nil {
		return // notification: no response
	}
	resp := rpcMessage{JSONRPC: "2.0", ID: msg.ID}
	if err != nil {
		var rerr *rpcError
		if !errors.As(err, &rerr) {
			rerr = &rpcError{Code: codeInternalError, Message: err.Error()}
		}
		resp.Error = rerr
	} else {
		raw, merr := json.Marshal(result)
		if merr != nil {
			resp.Error = &rpcError{Code: codeInternalError, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	}
	// A write failure means the stream is going down; the read loop will
	// surface it.
	_ = c.write(resp)
}

// call sends a request and decodes the peer's result into result (which may
// be nil to discard it).
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcMessage, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()

	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := c.write(rpcMessage{JSONRPC: "2.0", ID: &id, Method: method, Params: raw}); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("plugin: connection closed: %w", c.closeErr)
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// notify sends a notification (no response expected).
func (c *conn) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *conn) write(msg rpcMessage) error {
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
