package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"sync"
)

// MaxLine is the longest line that a connection reads. A longer line is
// skipped and answered with a parse error.
const MaxLine = 64 << 20

// Standard JSON-RPC and ACP error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeCancelled      = -32800
)

// ErrClosed is the error of a call that was pending when the connection ended.
var ErrClosed = errors.New("rpc: connection closed")

// Error is a JSON-RPC error. A handler returns it to choose the code of its
// error response; any other error becomes CodeInternal with a fixed message.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error returns the message with its code.
func (e *Error) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Handler serves the messages of one connection. Request runs in its own
// goroutine for each request; its result or error becomes the response.
// Notification runs in the reader goroutine, so it must not block.
type Handler interface {
	Request(ctx context.Context, method string, params json.RawMessage) (any, error)
	Notification(ctx context.Context, method string, params json.RawMessage)
}

// Closer is an optional interface of a Handler. Serve calls EndOfInput once,
// when it stops reading, before it waits for the requests that run, so the
// handler can end the work of those requests.
type Closer interface{ EndOfInput() }

// Admitter is an optional interface of a Handler. Serve calls Admit in the
// reader goroutine, in wire order, before it starts the goroutine of a request.
// The context that Admit returns goes to Request. Admit must not block.
type Admitter interface {
	Admit(ctx context.Context, method string, params json.RawMessage) context.Context
}

// Follower is an optional interface of a result. Serve calls AfterSent once,
// in the goroutine of the request, after it queued the response. A message that
// AfterSent queues reaches the peer after the response.
type Follower interface{ AfterSent() }

// Conn is one newline-delimited JSON-RPC 2.0 connection. One goroutine reads,
// one goroutine writes, and each request runs in its own goroutine.
type Conn struct {
	log     *slog.Logger
	out     chan []byte
	r       io.Reader
	maxLine int

	quit  chan struct{}
	wdone chan struct{}
	werr  error

	mu      sync.Mutex
	running map[string]context.CancelCauseFunc
	pending map[int64]chan reply
	nextID  int64
	closed  bool
}

type reply struct {
	result json.RawMessage
	err    error
}

// New returns a connection that reads r and writes w. A nil logger discards
// the diagnostics of the connection.
func New(r io.Reader, w io.Writer, logger *slog.Logger) *Conn {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	c := &Conn{
		log:     logger,
		r:       r,
		maxLine: MaxLine,
		out:     make(chan []byte, 64),
		quit:    make(chan struct{}),
		wdone:   make(chan struct{}),
		running: map[string]context.CancelCauseFunc{},
		pending: map[int64]chan reply{},
	}
	go c.write(w)
	return c
}

func (c *Conn) write(w io.Writer) {
	defer close(c.wdone)
	put := func(b []byte) {
		if c.werr != nil {
			return
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			c.werr = err
		}
	}
	for {
		select {
		case b := <-c.out:
			put(b)
		case <-c.quit:
			for {
				select {
				case b := <-c.out:
					put(b)
				default:
					return
				}
			}
		}
	}
}

// message is the union of the four JSON-RPC message shapes.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  *string         `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *Error          `json:"error"`
}

type wireError struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *Error          `json:"error"`
}

type wireResult struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

func (c *Conn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		c.log.Error("rpc: encoding a message failed", "error", err)
		return
	}
	select {
	case c.out <- b:
	case <-c.wdone:
	}
}

func (c *Conn) sendError(id json.RawMessage, e *Error) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	c.send(wireError{JSONRPC: "2.0", ID: id, Error: e})
}

// Notify sends a notification. The message is queued behind every message
// that the caller sent before, so order is the order of the calls.
func (c *Conn) Notify(method string, params any) error {
	return c.sendRequest(nil, method, params)
}

func (c *Conn) sendRequest(id *int64, method string, params any) error {
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		frame["id"] = *id
	}
	if params != nil {
		frame["params"] = params
	}
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	select {
	case <-c.wdone:
		return ErrClosed
	default:
	}
	select {
	case c.out <- b:
		return nil
	case <-c.wdone:
		return ErrClosed
	}
}

// Call sends a request to the peer and waits for its response. A non-nil
// result receives the decoded result. A peer error returns as *Error.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.sendRequest(&id, method, params); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if result != nil {
			return json.Unmarshal(r.result, result)
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type line struct {
	text []byte
	long bool
	err  error
}

// readLines splits r into lines. A line over MaxLine is dropped and reported
// as long. A line is over the bound when it holds more than maxLine bytes. An
// empty line is skipped.
func readLines(r io.Reader, maxLine int, stop <-chan struct{}, out chan<- line) {
	defer close(out)
	emit := func(l line) bool {
		select {
		case out <- l:
			return true
		case <-stop:
			return false
		}
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !long {
			buf = append(buf, chunk...)
			long = len(buf) > maxLine
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if long {
			if !emit(line{long: true}) {
				return
			}
		} else if text := trimLine(buf); len(text) > 0 {
			if !emit(line{text: text}) {
				return
			}
		}
		buf, long = nil, false
		if err != nil {
			if !errors.Is(err, io.EOF) {
				emit(line{err: err})
			}
			return
		}
	}
}

func trimLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// Serve reads messages until the input ends or ctx ends, calls h for each, and
// returns after every handler has returned and every response is written. It
// returns nil at the end of the input, the error of ctx when ctx ends first, and
// the write error when the output failed and nothing else did.
// Handlers of requests that are running see their context cancelled when ctx
// ends, and not at the end of the input.
func (c *Conn) Serve(ctx context.Context, h Handler) error {
	lines := make(chan line)
	stop := make(chan struct{})
	go readLines(c.r, c.maxLine, stop, lines)
	var wg sync.WaitGroup
	err := c.loop(ctx, h, lines, &wg)
	close(stop)
	if cl, ok := h.(Closer); ok {
		cl.EndOfInput()
	}
	wg.Wait()
	c.shutdown()
	if err == nil {
		err = c.werr
	}
	return err
}

func (c *Conn) loop(ctx context.Context, h Handler, lines <-chan line, wg *sync.WaitGroup) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case l, ok := <-lines:
			if !ok {
				return nil
			}
			switch {
			case l.err != nil:
				return l.err
			case l.long:
				c.sendError(nil, &Error{Code: CodeParseError, Message: "line too long"})
			default:
				c.dispatch(ctx, h, l.text, wg)
			}
		}
	}
}

func (c *Conn) shutdown() {
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		deliver(ch, reply{err: ErrClosed})
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.quit)
	<-c.wdone
}

// deliver hands r to the call that waits on ch. The channel holds one reply, so
// a second reply to the same call is dropped and never blocks the reader.
func deliver(ch chan<- reply, r reply) {
	select {
	case ch <- r:
	default:
	}
}

// idKey returns a key that is equal for equal ids, so 1 and 1.0 match. An id
// is a string, a number, or null; anything else reports false.
func idKey(raw json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	switch x := v.(type) {
	case nil:
		return "null", true
	case string:
		return "s:" + x, true
	case float64:
		return "n:" + strconv.FormatFloat(x, 'g', -1, 64), true
	}
	return "", false
}

func (c *Conn) dispatch(ctx context.Context, h Handler, text []byte, wg *sync.WaitGroup) {
	var m message
	if err := json.Unmarshal(text, &m); err != nil {
		code, msg := CodeParseError, "Parse error"
		if json.Valid(text) {
			code, msg = CodeInvalidRequest, "Invalid Request"
		}
		c.sendError(nil, &Error{Code: code, Message: msg})
		return
	}
	if _, ok := idKey(m.ID); len(m.ID) > 0 && !ok {
		c.sendError(nil, &Error{Code: CodeInvalidRequest, Message: "Invalid Request"})
		return
	}
	switch {
	case m.JSONRPC != "2.0":
		c.sendError(m.ID, &Error{Code: CodeInvalidRequest, Message: "Invalid Request"})
	case m.Method == nil && m.Result == nil && m.Error == nil:
		c.sendError(m.ID, &Error{Code: CodeInvalidRequest, Message: "Invalid Request"})
	case m.Method == nil:
		c.response(m)
	case len(m.ID) == 0:
		if *m.Method == "$/cancel_request" {
			c.cancelRequest(m.Params)
			return
		}
		h.Notification(ctx, *m.Method, m.Params)
	default:
		c.start(ctx, h, m, wg)
	}
}

func (c *Conn) response(m message) {
	var f float64
	if err := json.Unmarshal(m.ID, &f); err != nil || f != math.Trunc(f) {
		return
	}
	id := int64(f)
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		r := reply{result: m.Result}
		if m.Error != nil {
			r.err = m.Error
		}
		deliver(ch, r)
	}
}

func (c *Conn) cancelRequest(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	key, ok := idKey(p.RequestID)
	if !ok {
		return
	}
	c.mu.Lock()
	cancel := c.running[key]
	c.mu.Unlock()
	if cancel != nil {
		cancel(errRequestCancelled)
	}
}

var errRequestCancelled = errors.New("rpc: request cancelled by the peer")

func (c *Conn) start(ctx context.Context, h Handler, m message, wg *sync.WaitGroup) {
	key, _ := idKey(m.ID)
	rctx, cancel := context.WithCancelCause(ctx)
	c.mu.Lock()
	if _, busy := c.running[key]; busy {
		c.mu.Unlock()
		cancel(nil)
		c.sendError(m.ID, &Error{Code: CodeInvalidRequest, Message: "Invalid Request", Data: "a request with this id is running"})
		return
	}
	c.running[key] = cancel
	c.mu.Unlock()
	if ad, ok := h.(Admitter); ok {
		rctx = ad.Admit(rctx, *m.Method, m.Params)
	}
	wg.Go(func() {
		defer func() {
			c.mu.Lock()
			delete(c.running, key)
			c.mu.Unlock()
			cancel(nil)
		}()
		result, err := c.invoke(rctx, h, *m.Method, m.Params)
		switch {
		case err != nil && errors.Is(context.Cause(rctx), errRequestCancelled):
			c.sendError(m.ID, &Error{Code: CodeCancelled, Message: "Request cancelled"})
		case err != nil:
			c.sendError(m.ID, toError(err))
		default:
			if result == nil {
				result = struct{}{}
			}
			c.send(wireResult{JSONRPC: "2.0", ID: m.ID, Result: result})
			if f, ok := result.(Follower); ok {
				c.afterSent(f)
			}
		}
	})
}

// afterSent runs f.AfterSent and recovers a panic, as invoke does.
func (c *Conn) afterSent(f Follower) {
	defer func() {
		if p := recover(); p != nil {
			c.log.Error("rpc: AfterSent panicked", "panic", fmt.Sprint(p))
		}
	}()
	f.AfterSent()
}

func (c *Conn) invoke(ctx context.Context, h Handler, method string, params json.RawMessage) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			c.log.Error("rpc: handler panicked", "method", method, "panic", fmt.Sprint(p))
			err = &Error{Code: CodeInternal, Message: "Internal error"}
		}
	}()
	return h.Request(ctx, method, params)
}

func toError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: "Internal error"}
}
