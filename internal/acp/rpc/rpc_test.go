package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

type handlerFunc func(ctx context.Context, method string, params json.RawMessage) (any, error)

func (f handlerFunc) Request(ctx context.Context, method string, params json.RawMessage) (any, error) {
	return f(ctx, method, params)
}

func (f handlerFunc) Notification(context.Context, string, json.RawMessage) {}

type recorder struct {
	handlerFunc
	notes chan string
}

func (r *recorder) Notification(_ context.Context, method string, _ json.RawMessage) {
	r.notes <- method
}

// peer is the far end of a connection under test.
type peer struct {
	t     *testing.T
	in    *io.PipeWriter
	lines *bufio.Scanner
	done  chan error
	conn  *Conn
}

func startPeer(t *testing.T, h Handler) *peer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &peer{t: t, in: inW, lines: bufio.NewScanner(outR), done: make(chan error, 1)}
	p.lines.Buffer(nil, 1<<20)
	p.conn = New(inR, outW, nil)
	p.conn.maxLine = 1024
	go func() {
		err := p.conn.Serve(t.Context(), h)
		_ = outW.Close()
		p.done <- err
	}()
	return p
}

func (p *peer) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, line+"\n"); err != nil {
		p.t.Fatal(err)
	}
}

func (p *peer) next() map[string]any {
	p.t.Helper()
	if !p.lines.Scan() {
		p.t.Fatalf("output ended: %v", p.lines.Err())
	}
	var m map[string]any
	if err := json.Unmarshal(p.lines.Bytes(), &m); err != nil {
		p.t.Fatalf("output line %q is not JSON: %v", p.lines.Text(), err)
	}
	return m
}

func (p *peer) finish() {
	p.t.Helper()
	_ = p.in.Close()
	if err := <-p.done; err != nil {
		p.t.Errorf("Serve = %v, want nil at the end of the input", err)
	}
}

func echo(_ context.Context, method string, _ json.RawMessage) (any, error) {
	return map[string]string{"method": method}, nil
}

func code(m map[string]any) float64 {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

func TestBadJSONGetsParseErrorAndConnectionStaysOpen(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	p.send(`{"jsonrpc":"2.0","id":1,`)
	if got := p.next(); code(got) != CodeParseError || got["id"] != nil {
		t.Errorf("bad JSON answer = %v, want -32700 with a null id", got)
	}
	p.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if got := p.next(); got["id"] != float64(2) || got["result"] == nil {
		t.Errorf("request after bad JSON answer = %v", got)
	}
	p.finish()
}

func TestInvalidRequestShapesGetInvalidRequest(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	for _, line := range []string{`[1]`, `"x"`, `{"id":3,"method":"m"}`, `{"jsonrpc":"2.0","id":4}`} {
		p.send(line)
		if got := p.next(); code(got) != CodeInvalidRequest {
			t.Errorf("%s answer = %v, want -32600", line, got)
		}
	}
	p.finish()
}

func TestEmptyLinesAreSkipped(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	p.send("")
	p.send("   ")
	p.send(`{"jsonrpc":"2.0","id":"a","method":"ping"}`)
	if got := p.next(); got["id"] != "a" {
		t.Errorf("answer = %v, want the response to id a first", got)
	}
	p.finish()
}

func TestLongLineIsSkippedAndAnswered(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	p.send(`{"jsonrpc":"2.0","id":1,"method":"` + strings.Repeat("x", 4096) + `"}`)
	if got := p.next(); code(got) != CodeParseError {
		t.Errorf("long line answer = %v, want -32700", got)
	}
	p.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if got := p.next(); got["id"] != float64(2) {
		t.Errorf("answer after a long line = %v", got)
	}
	p.finish()
}

func TestUnknownIDKindsAreEchoed(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	p.send(`{"jsonrpc":"2.0","id":"s-1","method":"a"}`)
	if got := p.next(); got["id"] != "s-1" {
		t.Errorf("string id answer = %v", got)
	}
	p.finish()
}

func TestHandlerErrorsMapToCodes(t *testing.T) {
	p := startPeer(t, handlerFunc(func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		switch method {
		case "typed":
			return nil, &Error{Code: CodeInvalidParams, Message: "bad"}
		case "panic":
			panic("boom")
		}
		return nil, io.ErrUnexpectedEOF
	}))
	for method, want := range map[string]float64{"typed": CodeInvalidParams, "panic": CodeInternal, "plain": CodeInternal} {
		p.send(`{"jsonrpc":"2.0","id":1,"method":"` + method + `"}`)
		got := p.next()
		if code(got) != want {
			t.Errorf("%s answer = %v, want code %v", method, got, want)
		}
		if strings.Contains(p.lines.Text(), "boom") || strings.Contains(p.lines.Text(), "EOF") {
			t.Errorf("%s answer leaks the error text: %s", method, p.lines.Text())
		}
	}
	p.finish()
}

type heldAfterSent struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (h heldAfterSent) AfterSent() {
	h.entered <- struct{}{}
	<-h.release
}

func TestRequestIDIsFreeOnceItsAnswerIsSent(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	p := startPeer(t, handlerFunc(func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "first" {
			return heldAfterSent{entered: entered, release: release}, nil
		}
		return map[string]string{"method": method}, nil
	}))
	p.send(`{"jsonrpc":"2.0","id":1,"method":"first"}`)
	if got := p.next(); got["id"] != float64(1) || got["error"] != nil {
		t.Fatalf("first answer = %v, want a result", got)
	}
	<-entered
	p.send(`{"jsonrpc":"2.0","id":1,"method":"second"}`)
	got := p.next()
	res, _ := got["result"].(map[string]any)
	if got["error"] != nil || got["id"] != float64(1) || res["method"] != "second" {
		t.Errorf("answer of a request that reuses an answered id = %v, want the result of second", got)
	}
	close(release)
	p.finish()
}

func TestNotificationsGetNoResponse(t *testing.T) {
	r := &recorder{handlerFunc: echo, notes: make(chan string, 1)}
	p := startPeer(t, r)
	p.send(`{"jsonrpc":"2.0","method":"note"}`)
	if got := <-r.notes; got != "note" {
		t.Errorf("notification = %q", got)
	}
	p.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if got := p.next(); got["id"] != float64(1) {
		t.Errorf("the first output is %v, want the response to the request", got)
	}
	p.finish()
}

func TestCancelRequestAnswersCancelled(t *testing.T) {
	started := make(chan struct{})
	p := startPeer(t, handlerFunc(func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	p.send(`{"jsonrpc":"2.0","id":7,"method":"slow"}`)
	<-started
	p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":7}}`)
	got := p.next()
	if got["id"] != float64(7) || code(got) != CodeCancelled {
		t.Errorf("answer = %v, want -32800 for id 7", got)
	}
	p.finish()
}

func TestCancelRequestForUnknownOrFinishedIDIsIgnored(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":99}}`)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if got := p.next(); got["id"] != float64(1) || got["result"] == nil {
		t.Errorf("answer = %v", got)
	}
	p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`)
	p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":"bad"}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if got := p.next(); got["id"] != float64(2) || got["result"] == nil {
		t.Errorf("answer = %v", got)
	}
	p.finish()
}

func TestServeWaitsForRunningHandlerAtEndOfInput(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	p := startPeer(t, handlerFunc(func(context.Context, string, json.RawMessage) (any, error) {
		close(started)
		<-release
		return "late", nil
	}))
	p.send(`{"jsonrpc":"2.0","id":1,"method":"slow"}`)
	<-started
	_ = p.in.Close()
	close(release)
	if got := p.next(); got["result"] != "late" {
		t.Errorf("answer = %v, want the result after the input ended", got)
	}
	if err := <-p.done; err != nil {
		t.Errorf("Serve = %v", err)
	}
}

func TestConcurrentNotifiesWriteWholeLines(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	const senders, each = 8, 50
	var wg sync.WaitGroup
	for range senders {
		wg.Go(func() {
			for range each {
				if err := p.conn.Notify("tick", map[string]string{"text": "a\nb"}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	for range senders * each {
		if got := p.next(); got["method"] != "tick" {
			t.Fatalf("line = %v", got)
		}
	}
	wg.Wait()
	p.finish()
}

func TestNotificationPrecedesLaterResponse(t *testing.T) {
	var conn *Conn
	p := startPeer(t, handlerFunc(func(context.Context, string, json.RawMessage) (any, error) {
		_ = conn.Notify("update", nil)
		return "done", nil
	}))
	conn = p.conn
	p.send(`{"jsonrpc":"2.0","id":1,"method":"prompt"}`)
	if first := p.next(); first["method"] != "update" {
		t.Errorf("first line = %v, want the notification", first)
	}
	if second := p.next(); second["result"] != "done" {
		t.Errorf("second line = %v, want the response", second)
	}
	p.finish()
}

func TestCallMatchesResponseByID(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	var out struct{ Answer int }
	errc := make(chan error, 1)
	go func() { errc <- p.conn.Call(context.Background(), "ask", nil, &out) }()
	req := p.next()
	if req["method"] != "ask" || req["id"] == nil {
		t.Fatalf("request = %v", req)
	}
	p.send(`{"jsonrpc":"2.0","id":99,"result":{"Answer":1}}`)
	p.send(`{"jsonrpc":"2.0","id":` + string(mustJSON(t, req["id"])) + `,"result":{"Answer":42}}`)
	if err := <-errc; err != nil || out.Answer != 42 {
		t.Errorf("Call = %v, answer %d, want 42", err, out.Answer)
	}
	p.finish()
}

func TestCallReturnsPeerErrorAndFailsAtClose(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	errc := make(chan error, 1)
	go func() { errc <- p.conn.Call(context.Background(), "ask", nil, nil) }()
	req := p.next()
	p.send(`{"jsonrpc":"2.0","id":` + string(mustJSON(t, req["id"])) + `,"error":{"code":-32602,"message":"no"}}`)
	if e, ok := (<-errc).(*Error); !ok || e.Code != CodeInvalidParams {
		t.Errorf("Call error = %v, want the peer error", e)
	}
	go func() { errc <- p.conn.Call(context.Background(), "ask", nil, nil) }()
	p.next()
	p.finish()
	if err := <-errc; err == nil {
		t.Error("a call pending at the end of the connection returned nil")
	}
	if err := p.conn.Notify("late", nil); err == nil {
		t.Error("Notify after the connection ended returned nil")
	}
}

func TestServeStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	c := New(inR, io.Discard, nil)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx, handlerFunc(echo)) }()
	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("Serve = %v, want context.Canceled", err)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type followed struct{ conn *Conn }

func (f followed) AfterSent() { _ = f.conn.Notify("after", nil) }

func TestFollowerMessageReachesThePeerAfterTheResponse(t *testing.T) {
	var p *peer
	p = startPeer(t, handlerFunc(func(context.Context, string, json.RawMessage) (any, error) {
		return followed{conn: p.conn}, nil
	}))
	p.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if got := p.next(); got["id"] != float64(1) {
		t.Errorf("first message = %v, want the response", got)
	}
	if got := p.next(); got["method"] != "after" {
		t.Errorf("second message = %v, want the follower notification", got)
	}
	p.finish()
}

type followedError struct{ conn *Conn }

func (followedError) Error() string { return "failed" }
func (f followedError) AfterSent()  { _ = f.conn.Notify("after", nil) }

func TestFollowerErrorMessageReachesThePeerAfterTheErrorResponse(t *testing.T) {
	var p *peer
	p = startPeer(t, handlerFunc(func(context.Context, string, json.RawMessage) (any, error) {
		return nil, followedError{conn: p.conn}
	}))
	p.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if got := p.next(); got["id"] != float64(1) || got["error"] == nil {
		t.Errorf("first message = %v, want the error response", got)
	}
	if got := p.next(); got["method"] != "after" {
		t.Errorf("second message = %v, want the follower notification", got)
	}
	p.finish()
}
