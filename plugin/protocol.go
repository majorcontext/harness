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
type conn struct {
	wmu sync.Mutex // serializes writes
	rwc io.ReadWriteCloser
	r   *bufio.Reader

	handler handlerFunc
	nextID  atomic.Int64

	pmu     sync.Mutex
	pending map[int64]chan rpcMessage

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

func newConn(rwc io.ReadWriteCloser, handler handlerFunc) *conn {
	return &conn{
		rwc:     rwc,
		r:       bufio.NewReader(rwc),
		handler: handler,
		pending: make(map[int64]chan rpcMessage),
		closed:  make(chan struct{}),
	}
}

// run reads and dispatches messages until the stream ends. Incoming requests
// are served on their own goroutines so that a handler blocked on an outgoing
// call (e.g. a hook making a client API request) never stalls the read loop.
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
