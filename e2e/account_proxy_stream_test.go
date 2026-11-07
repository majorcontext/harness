package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type accountProxyTransport struct {
	base    http.RoundTripper
	capture *accountProxyCapture
}

func (p accountProxyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	p.capture.mu.Lock()
	if p.capture.started.IsZero() {
		p.capture.started = start
	}
	p.capture.requests++
	request, origin := p.capture.requests, p.capture.started
	p.capture.mu.Unlock()
	var once sync.Once
	record := func(phase string, err error) {
		once.Do(func() {
			state := "active"
			if r.Context().Err() != nil {
				state = "canceled"
				if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
					state = "deadline"
				}
			}
			line := fmt.Sprintf("account proxy request=%d phase=%s at=%s after=%s error_type=%T eof=%t unexpected_eof=%t reset=%t closed=%t context=%s", request, phase, time.Since(origin), time.Since(start), err, errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrBodyReadAfterClose), state)
			p.capture.mu.Lock()
			p.capture.failures = append(p.capture.failures, line)
			p.capture.mu.Unlock()
		})
	}
	resp, err := p.base.RoundTrip(r)
	if err != nil {
		record("transport", err)
	} else if resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body = &accountProxyStream{ReadCloser: resp.Body, tail: "\n", record: record}
	}
	return resp, err
}

type accountProxyStream struct {
	io.ReadCloser
	tail     string
	complete bool
	record   func(string, error)
}

func (b *accountProxyStream) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.complete {
		data := b.tail + string(p[:n])
		b.complete = strings.Contains(data, "\ndata: {\"type\":\"message_stop\"}\n\n") || strings.Contains(data, "\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n")
		b.tail = data[max(0, len(data)-48):]
	}
	if err != nil && (!b.complete || !errors.Is(err, io.EOF)) {
		b.record("stream", err)
	}
	return n, err
}

func (p *accountProxyCapture) diagnosticLines(failed bool) []string {
	if !failed {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.failures)
}
