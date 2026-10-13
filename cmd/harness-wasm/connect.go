//go:build js

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"syscall/js"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/acp/cwdruntimes"
	"github.com/majorcontext/harness/internal/jshost"
	"github.com/majorcontext/harness/store"
)

// options is what a host passes to harnessConnect.
type options struct {
	user         *config.Config
	err          error
	storeDir     string
	defaultDir   string
	env          map[string]string
	builtinTools []string
	tools        []harness.Tool
	proxyHeaders map[string]string
	proxyBase    string
	onFrame      js.Value
	logger       *slog.Logger
}

// readOptions reads {config, storeDir, defaultDir, env, tools, builtinTools, proxy: {headers, baseURL},
// onFrame, onLog}. Every option that is present must have its type. A bad value
// does not throw: the first error fails the first session/new as an invalid
// request.
func readOptions(o js.Value) *options {
	opts := &options{onFrame: o.Get("onFrame"), logger: slog.New(slog.DiscardHandler)}
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	opts.storeDir, err = jshost.StringField(o, "storeDir")
	keep(err)
	opts.defaultDir, err = jshost.StringField(o, "defaultDir")
	keep(err)
	opts.env, err = jshost.StringMap("env", o.Get("env"))
	keep(err)
	opts.builtinTools, err = jshost.StringList("builtinTools", o.Get("builtinTools"))
	keep(err)
	opts.tools, err = jshost.ReadTools(o.Get("tools"))
	keep(err)
	proxy := o.Get("proxy")
	if proxy.Type() != js.TypeUndefined && proxy.Type() != js.TypeNull && proxy.Type() != js.TypeObject {
		keep(errors.New("proxy must be an object"))
	} else {
		opts.proxyHeaders, err = jshost.StringMap("proxy.headers", jshost.Object(proxy, "headers"))
		keep(err)
		if proxy.Type() == js.TypeObject {
			opts.proxyBase, err = jshost.StringField(proxy, "baseURL")
			keep(err)
		}
	}
	if fn := o.Get("onLog"); fn.Type() == js.TypeFunction {
		opts.logger = slog.New(slog.NewJSONHandler(jshost.LogWriter{Fn: fn}, nil))
	}
	opts.user, err = readConfig(o.Get("config"))
	keep(err)
	opts.err = errors.Join(errs...)
	return opts
}

// readConfig reads the user config: an object, or an absent value for an empty config.
func readConfig(c js.Value) (*config.Config, error) {
	text := "{}"
	switch c.Type() {
	case js.TypeUndefined, js.TypeNull:
	case js.TypeObject:
		var err error
		if text, err = jshost.JSONText(c); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
	default:
		return nil, errors.New("config must be an object")
	}
	return config.Parse("the config of harnessConnect", []byte(text))
}

// proxyOrigin is the scheme and host of proxy.baseURL, or empty when there is none.
func (o *options) proxyOrigin() string {
	u, err := url.Parse(o.proxyBase)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// runtimes builds the Runtimes of the connection: one Runtime for each work
// directory over one store and one local owner.
func (o *options) runtimes() *cwdruntimes.Runtimes {
	return cwdruntimes.New(cwdruntimes.Options{
		Config: func(cwd string) (*config.Config, error) {
			if o.err != nil {
				return nil, o.err
			}
			cfg, err := config.MergeProject(o.user, "the config of harnessConnect", cwd)
			if err != nil {
				return nil, err
			}
			out := cfg.WithProxyBase(o.proxyBase)
			return &out, nil
		},
		Shared: func(*config.Config) (harness.Store, harness.Owner, error) {
			if o.storeDir == "" {
				return store.NewMemStore(), harness.NewLocalOwner(1), nil
			}
			return store.NewDiskStore(o.storeDir), harness.NewLocalOwner(1), nil
		},
		DefaultDir: o.defaultDir,
		Version:    "wasm",
		Getenv:     func(k string) string { return o.env[k] },
		Logger:     o.logger,
		ModelTransport: func(string) http.RoundTripper {
			return jshost.FetchRT{Headers: o.proxyHeaders, Origin: o.proxyOrigin()}
		},
		Tools: o.tools,
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
	queue := jshost.NewFrameQueue()
	go queue.Drain(inW)
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		jshost.PumpFrames(outR, opts.onFrame, func(err error) { opts.logger.Error("onFrame", "error", err.Error()) })
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cfg := acp.Config{Version: "wasm", Logger: opts.logger, AllowedTools: opts.builtinTools, List: opts.defaultDir != ""}
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
			queue.Push(a[0].String())
		}
		return nil
	}))
	handle.Set("close", js.FuncOf(func(js.Value, []js.Value) any {
		queue.Close()
		return jshost.SettleWhen(done)
	}))
	return handle
}
