//go:build js

package jshost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall/js"
)

// FetchError is a fetch that got no response: a CORS block, a DNS or TLS
// failure, an unreachable host, or an offline browser. JavaScript cannot tell
// them apart. It holds no query, header, or body.
type FetchError struct {
	Method, Origin string
	// Name and Message are the name and message of the JavaScript error.
	Name, Message string
	// Online is navigator.onLine at the failure.
	Online bool
}

func (e *FetchError) Error() string {
	return fmt.Sprintf("fetch %s %s: %s; the browser blocked the request or the host is unreachable (CORS, DNS, TLS, or offline)",
		e.Method, e.Origin, (&JSError{Name: e.Name, Message: e.Message}).Error())
}

// Permanent says that no retry can help while the host reports online: a
// request that gets no response then fails the same way again. A host that
// reports offline, or reports nothing, keeps the retry.
func (e *FetchError) Permanent() bool { return e.Online }

// FetchRT is the http.RoundTripper of the model transport. It calls the fetch
// of the global scope, so the Worker or Node supplies the network. headers go
// on every request unless the request sets them.
type FetchRT struct{ Headers map[string]string }

func online() bool {
	nav := js.Global().Get("navigator")
	if nav.Type() != js.TypeObject {
		return false
	}
	v := nav.Get("onLine")
	return v.Type() == js.TypeBoolean && v.Bool()
}

// RoundTrip sends req through fetch and returns the response with a streaming body.
func (f FetchRT) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	opt := js.Global().Get("Object").New()
	opt.Set("method", req.Method)
	h := js.Global().Get("Object").New()
	for k, v := range f.Headers {
		if req.Header.Get(k) == "" {
			h.Set(k, v)
		}
	}
	for k, vs := range req.Header {
		h.Set(k, strings.Join(vs, ", "))
	}
	opt.Set("headers", h)
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(b) > 0 {
			u8 := js.Global().Get("Uint8Array").New(len(b))
			js.CopyBytesToJS(u8, b)
			opt.Set("body", u8)
		}
	}
	ac := js.Global().Get("AbortController").New()
	opt.Set("signal", ac.Get("signal"))
	stop := context.AfterFunc(ctx, func() { ac.Call("abort") })
	res, err := Await(ctx, js.Global().Call("fetch", req.URL.String(), opt))
	if err != nil {
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if je, ok := errors.AsType[*JSError](err); ok {
			return nil, &FetchError{Method: req.Method, Origin: req.URL.Scheme + "://" + req.URL.Host,
				Name: je.Name, Message: je.Message, Online: online()}
		}
		return nil, err
	}
	hdr := http.Header{}
	it := res.Get("headers").Call("entries")
	for {
		n := it.Call("next")
		if n.Get("done").Bool() {
			break
		}
		p := n.Get("value")
		hdr.Add(p.Index(0).String(), p.Index(1).String())
	}
	var rc io.ReadCloser = http.NoBody
	if body := res.Get("body"); body.Truthy() {
		rc = &streamBody{ctx: ctx, r: body.Call("getReader"), stop: stop}
	} else {
		stop()
	}
	status := res.Get("status").Int()
	return &http.Response{Status: fmt.Sprintf("%d %s", status, res.Get("statusText").String()),
		StatusCode: status, Header: hdr, Body: rc, Request: req, ContentLength: -1,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
}

// streamBody reads a fetch response. Its abort stays tied to the request
// context until Close, so a cancel ends a stream that has begun.
type streamBody struct {
	ctx     context.Context
	r       js.Value
	pending []byte
	err     error
	stop    func() bool
}

func (s *streamBody) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if len(s.pending) == 0 {
		v, err := Await(s.ctx, s.r.Call("read"))
		if err != nil {
			if s.ctx.Err() != nil {
				err = s.ctx.Err()
			} else if je, ok := errors.AsType[*JSError](err); ok {
				err = fmt.Errorf("fetch: reading the response: %w", je)
			}
			s.err = err
			return 0, err
		}
		if v.Get("done").Bool() {
			s.err = io.EOF
			return 0, io.EOF
		}
		chunk := v.Get("value")
		s.pending = make([]byte, chunk.Get("byteLength").Int())
		js.CopyBytesToGo(s.pending, chunk)
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *streamBody) Close() error {
	s.stop()
	discard(s.r.Call("cancel"))
	if s.err == nil {
		s.err = http.ErrBodyReadAfterClose
	}
	return nil
}

// discard ends the interest in a promise, so a rejection of it is not an
// unhandled rejection of the host.
func discard(p js.Value) {
	var f js.Func
	f = js.FuncOf(func(js.Value, []js.Value) any {
		go f.Release()
		return nil
	})
	p.Call("then", f, f)
}
