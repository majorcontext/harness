package rpc

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestServeReturnsWriteErrorAtEndOfInput(t *testing.T) {
	want := errors.New("stdout closed")
	c := New(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), failingWriter{want}, nil)
	if err := c.Serve(t.Context(), handlerFunc(echo)); !errors.Is(err, want) {
		t.Errorf("Serve = %v, want the write error", err)
	}
}

func TestReadLinesStopsWhenStopCloses(t *testing.T) {
	stop := make(chan struct{})
	out := make(chan line)
	done := make(chan struct{})
	go func() {
		readLines(strings.NewReader("{}\n{}\n{}\n"), 1024, stop, out)
		close(done)
	}()
	<-out
	close(stop)
	<-done
}

func TestPendingCallFailsWithErrClosedAtEndOfInput(t *testing.T) {
	p := startPeer(t, handlerFunc(echo))
	errc := make(chan error, 1)
	go func() { errc <- p.conn.Call(context.Background(), "ask", nil, nil) }()
	p.next()
	p.finish()
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Errorf("Call = %v, want ErrClosed", err)
	}
}
