package rpc

import (
	"context"
	"encoding/json"
	"testing"
)

func TestIDOfOtherTypeGetsInvalidRequestWithNullID(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	for _, id := range []string{`{"a":1}`, `true`, `[1]`} {
		p.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
		if got := p.next(); code(got) != CodeInvalidRequest || got["id"] != nil {
			t.Errorf("id %s answer = %v, want -32600 with a null id", id, got)
		}
	}
	p.send(`{"jsonrpc":"2.0","id":{"a":1},"result":{}}`)
	if got := p.next(); code(got) != CodeInvalidRequest || got["id"] != nil {
		t.Errorf("response with an object id answer = %v", got)
	}
	p.finish()
}

func TestRequestWithNullIDIsAnswered(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	p.send(`{"jsonrpc":"2.0","id":null,"method":"ping"}`)
	if got := p.next(); got["id"] != nil || got["result"] == nil {
		t.Errorf("answer = %v, want a result with a null id", got)
	}
	p.finish()
}

func TestCancelRequestMatchesEqualNumbers(t *testing.T) {
	started := make(chan struct{})
	p := startPeer(t, handlerFunc(func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	p.send(`{"jsonrpc":"2.0","id":1,"method":"slow"}`)
	<-started
	p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1.0}}`)
	if got := p.next(); code(got) != CodeCancelled {
		t.Errorf("answer = %v, want -32800", got)
	}
	p.finish()
}

func TestRequestWithRunningIDIsRefused(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	p := startPeer(t, handlerFunc(func(context.Context, string, json.RawMessage) (any, error) {
		close(started)
		<-release
		return "first", nil
	}))
	p.send(`{"jsonrpc":"2.0","id":5,"method":"slow"}`)
	<-started
	p.send(`{"jsonrpc":"2.0","id":5.0,"method":"slow"}`)
	if got := p.next(); code(got) != CodeInvalidRequest {
		t.Errorf("second request answer = %v, want -32600", got)
	}
	close(release)
	if got := p.next(); got["result"] != "first" {
		t.Errorf("first request answer = %v", got)
	}
	p.finish()
}

func TestCallThatGaveUpAndDuplicateResponsesDoNotStallServe(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- p.conn.Call(ctx, "ask", nil, nil) }()
	id := string(mustJSON(t, p.next()["id"]))
	cancel()
	if err := <-errc; err == nil {
		t.Error("Call returned nil after its context ended")
	}
	for range 200 {
		p.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{}}`)
	}
	p.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if got := p.next(); got["id"] != float64(1) {
		t.Errorf("answer = %v", got)
	}
	p.finish()
}

func TestDuplicateResponsesToALiveCallDoNotStallServe(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	errc := make(chan error, 1)
	go func() { errc <- p.conn.Call(t.Context(), "ask", nil, nil) }()
	id := string(mustJSON(t, p.next()["id"]))
	for range 200 {
		p.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{}}`)
	}
	if err := <-errc; err != nil {
		t.Errorf("Call = %v", err)
	}
	p.finish()
}
