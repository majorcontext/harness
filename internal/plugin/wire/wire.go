// Package wire is the JSON-RPC 2.0 transport that the plugin host and the
// public plugin package share. It speaks one message per line (NDJSON) over a
// byte stream, in both directions at once.
package wire

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

// Method names. Hooks are dispatched as HookPrefix + the hook name.
const (
	MethodInitialize  = "initialize"
	MethodShutdown    = "shutdown"
	MethodToolExecute = "tool/execute"
	HookPrefix        = "hook/"

	// Plugin to harness client API.
	MethodSessionMessages = "client/session.messages"
	MethodMCPCall         = "client/mcp.call"
	MethodGenerate        = "client/generate"
)

// JSON-RPC 2.0 error codes.
const (
	CodeMethodNotFound = -32601
	CodeInternalError  = -32603
)

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("plugin: rpc error %d: %s", e.Code, e.Message)
}

// Message is a JSON-RPC 2.0 request, notification, or response. IDs are
// always numbers in this protocol.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Handler serves one incoming request or notification. Its ctx ends with the
// connection. Its result answers a request and is ignored for a notification.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// NotifyQueueSize bounds the backlog of droppable notifications (hook/event)
// per connection. A full queue drops them and never blocks the read loop.
const NotifyQueueSize = 1024

// Conn is a bidirectional JSON-RPC 2.0 connection over a byte stream.
type Conn struct {
	wmu sync.Mutex // serializes writes
	rwc io.ReadWriteCloser
	r   *bufio.Reader

	handler Handler
	nextID  atomic.Int64

	pmu     sync.Mutex
	pending map[int64]chan Message

	// notifyCh feeds the one goroutine that handles notifications in order.
	// The read loop never blocks on it; shutdown bypasses it.
	notifyCh chan Message

	notifyDropped atomic.Uint64

	reqs sync.WaitGroup // request handlers in flight and the notification loop

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error

	ctx    context.Context
	cancel context.CancelFunc
}

// NewConn returns a Conn over rwc. Call Run to start reading.
func NewConn(rwc io.ReadWriteCloser, handler Handler) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		ctx:      ctx,
		cancel:   cancel,
		rwc:      rwc,
		r:        bufio.NewReader(rwc),
		handler:  handler,
		pending:  make(map[int64]chan Message),
		notifyCh: make(chan Message, NotifyQueueSize),
		closed:   make(chan struct{}),
	}
	c.reqs.Go(c.runNotifications)
	return c
}

// runNotifications handles queued notifications in order until the Conn closes.
func (c *Conn) runNotifications() {
	for {
		select {
		case msg := <-c.notifyCh:
			select {
			case <-c.closed:
				return
			default:
			}
			c.serveRequest(msg)
		case <-c.closed:
			return
		}
	}
}

// Run reads and dispatches messages until the stream ends. Each request runs on
// its own goroutine, so a handler blocked on an outgoing call never stalls the
// read loop. Notifications keep their receipt order on one goroutine.
func (c *Conn) Run() error {
	for {
		line, err := c.r.ReadBytes('\n')
		if len(line) > 1 {
			var msg Message
			if uerr := json.Unmarshal(line, &msg); uerr == nil {
				c.dispatch(msg)
			}
			// A malformed line is dropped: the peer does not speak the protocol.
		}
		if err != nil {
			c.fail(err)
			return err
		}
	}
}

func (c *Conn) dispatch(msg Message) {
	if msg.Method != "" {
		if msg.ID == nil {
			c.dispatchNotification(msg)
			return
		}
		c.reqs.Go(func() { c.serveRequest(msg) })
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

// dispatchNotification never blocks the read loop, which also carries
// responses. Shutdown runs inline: losing it would make Run's caller mistake a
// stop for a read error. Other notifications queue, and a full queue drops them.
func (c *Conn) dispatchNotification(msg Message) {
	if msg.Method == MethodShutdown {
		c.serveRequest(msg)
		return
	}
	select {
	case c.notifyCh <- msg:
	default:
		c.notifyDropped.Add(1)
	}
}

func (c *Conn) serveRequest(msg Message) {
	result, err := c.handler(c.ctx, msg.Method, msg.Params)
	if msg.ID == nil {
		return
	}
	resp := Message{JSONRPC: "2.0", ID: msg.ID}
	if err != nil {
		var rerr *Error
		if !errors.As(err, &rerr) {
			rerr = &Error{Code: CodeInternalError, Message: err.Error()}
		}
		resp.Error = rerr
	} else {
		raw, merr := json.Marshal(result)
		if merr != nil {
			resp.Error = &Error{Code: CodeInternalError, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	}
	// A write failure means the stream is going down; the read loop will
	// surface it.
	_ = c.write(resp)
}

// Wait blocks until every handler has returned. Call it after Run returns.
func (c *Conn) Wait() { c.reqs.Wait() }

// Call sends a request and decodes the result into result, which may be nil.
// Concurrent calls are safe.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan Message, 1)
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
	if err := c.write(Message{JSONRPC: "2.0", ID: &id, Method: method, Params: raw}); err != nil {
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

// Notify sends a notification (no response expected).
func (c *Conn) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(Message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *Conn) write(msg Message) error {
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

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// NotifyDropped counts notifications that a saturated queue dropped.
func (c *Conn) NotifyDropped() uint64 { return c.notifyDropped.Load() }

func (c *Conn) fail(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		close(c.closed)
		c.cancel()
	})
}

// Close closes the stream and fails every pending call.
func (c *Conn) Close() error {
	c.fail(io.ErrClosedPipe)
	return c.rwc.Close()
}
