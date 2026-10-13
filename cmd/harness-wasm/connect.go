//go:build js

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"syscall/js"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/acp/cwdruntimes"
	"github.com/majorcontext/harness/store"
)

// options is what a host passes to harnessConnect.
type options struct {
	user         *config.Config
	err          error
	storeDir     string
	env          map[string]string
	builtinTools []string
	tools        []harness.Tool
	proxyHeaders map[string]string
	onFrame      js.Value
	logger       *slog.Logger
}

// readOptions reads {config, storeDir, env, tools, builtinTools, proxy: {headers},
// onFrame, onLog}. A bad value does not throw: it fails the first session/new
// as an invalid request.
func readOptions(o js.Value) *options {
	opts := &options{
		storeDir: stringField(o, "storeDir"),
		env:      stringMap(o.Get("env")),
		onFrame:  o.Get("onFrame"),
		logger:   slog.New(slog.DiscardHandler),
	}
	if fn := o.Get("onLog"); fn.Type() == js.TypeFunction {
		opts.logger = slog.New(slog.NewJSONHandler(logWriter{fn}, nil))
	}
	text := "{}"
	if c := o.Get("config"); c.Type() == js.TypeObject {
		var err error
		if text, err = jsonText(c); err != nil {
			opts.err = fmt.Errorf("config: %w", err)
		}
	}
	if opts.err == nil {
		opts.user, opts.err = config.Parse("the config of harnessConnect", []byte(text))
	}
	if opts.err == nil {
		opts.tools, opts.err = readTools(o.Get("tools"))
	}
	opts.builtinTools = stringList(o.Get("builtinTools"))
	if proxy := o.Get("proxy"); proxy.Type() == js.TypeObject {
		opts.proxyHeaders = stringMap(proxy.Get("headers"))
	}
	return opts
}

// logWriter sends each log line to the onLog function of the host.
type logWriter struct{ fn js.Value }

func (w logWriter) Write(p []byte) (int, error) {
	if err := call(w.fn, strings.TrimSuffix(string(p), "\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}

// runtimes builds the Runtimes of the connection: one Runtime for each work
// directory over one store and one local owner.
func (o *options) runtimes() *cwdruntimes.Runtimes {
	return cwdruntimes.New(cwdruntimes.Options{
		Config: func(cwd string) (*config.Config, error) {
			if o.err != nil {
				return nil, o.err
			}
			return config.MergeProject(o.user, "the config of harnessConnect", cwd)
		},
		Shared: func(*config.Config) (harness.Store, harness.Owner, error) {
			if o.storeDir == "" {
				return store.NewMemStore(), harness.NewLocalOwner(1), nil
			}
			return store.NewDiskStore(o.storeDir), harness.NewLocalOwner(1), nil
		},
		Version:        "wasm",
		Getenv:         func(k string) string { return o.env[k] },
		Logger:         o.logger,
		ModelTransport: func(string) http.RoundTripper { return fetchRT{headers: o.proxyHeaders} },
		Tools:          o.tools,
	})
}

// harnessConnect opens one ACP connection and returns {send, close}. send(frame)
// takes one ACP message and never blocks. onFrame(frame) receives each message
// of the agent. close() ends the input and returns a promise that settles when
// the sessions are released and the last frame is delivered.
//
// The handle outlives the connection, so its functions are never released; a
// call after the end is ignored.
func harnessConnect(_ js.Value, args []js.Value) any {
	o := js.Global().Get("Object").New()
	if len(args) > 0 && args[0].Type() == js.TypeObject {
		o = args[0]
	}
	opts := readOptions(o)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	queue := newFrameQueue()
	go queue.drain(inW)
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		pumpFrames(outR, opts.onFrame, func(err error) { opts.logger.Error("onFrame", "error", err.Error()) })
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cfg := acp.Config{Version: "wasm", Logger: opts.logger, AllowedTools: opts.builtinTools}
		if err := acp.Serve(context.Background(), opts.runtimes(), inR, outW, cfg); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			opts.logger.Error("acp", "error", err.Error())
		}
		_ = outW.Close()
		_ = inR.CloseWithError(io.ErrClosedPipe)
		<-outDone
	}()
	handle := js.Global().Get("Object").New()
	handle.Set("send", js.FuncOf(func(_ js.Value, a []js.Value) any {
		if len(a) > 0 && a[0].Type() == js.TypeString {
			queue.push(a[0].String())
		}
		return nil
	}))
	handle.Set("close", js.FuncOf(func(js.Value, []js.Value) any {
		queue.close()
		return settleWhen(done)
	}))
	return handle
}

// settleWhen returns a promise that resolves when done closes.
func settleWhen(done <-chan struct{}) js.Value {
	var executor js.Func
	executor = js.FuncOf(func(_ js.Value, a []js.Value) any {
		resolve := a[0]
		go func() {
			<-done
			resolve.Invoke()
		}()
		go executor.Release()
		return nil
	})
	return js.Global().Get("Promise").New(executor)
}
