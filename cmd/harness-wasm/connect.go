//go:build js

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os/exec"
	"syscall/js"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
	"github.com/majorcontext/harness/internal/acp/acpauth"
	"github.com/majorcontext/harness/internal/acp/cwdruntimes"
	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/injectedauth"
	"github.com/majorcontext/harness/internal/jshost"
	"github.com/majorcontext/harness/store"
)

// options is what a host passes to harnessConnect.
type options struct {
	// logins owns the login state of the injected provider entries, for the runtimes and the login of the connection.
	logins       *injectedauth.Logins
	user         *config.Config
	err          error
	storeDir     string
	defaultDir   string
	env          map[string]string
	builtinTools []string
	tools        []harness.Tool
	proxyHeaders map[string]string
	proxyBase    string
	// modelFetch carries the model requests of a page fetch. Absent: the global fetch.
	modelFetch js.Value
	// providers maps a provider name to the base_url and login_url that the host sets on its entry.
	providers map[string]providerURLs
	onFrame   js.Value
	logger    *slog.Logger
}

// providerURLs is what proxy.providers sets on a provider entry. An empty field sets nothing.
type providerURLs struct{ baseURL, loginURL string }

// readOptions reads {config, storeDir, defaultDir, env, tools, builtinTools, proxy: {headers, baseURL, providers: {name: {baseURL, loginURL}}},
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
	opts.modelFetch = o.Get("modelFetch")
	if t := opts.modelFetch.Type(); t != js.TypeUndefined && t != js.TypeNull && t != js.TypeFunction {
		keep(errors.New("modelFetch must be a function"))
	}
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
			opts.providers, err = readProviders(proxy.Get("providers"))
			keep(err)
		}
	}
	if fn := o.Get("onLog"); fn.Type() == js.TypeFunction {
		opts.logger = slog.New(slog.NewJSONHandler(jshost.LogWriter{Fn: fn}, nil))
	}
	opts.user, err = readConfig(o.Get("config"))
	keep(err)
	if opts.user != nil {
		keep(opts.applyHostConfig())
	}
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

// readProviders reads proxy.providers: an object that maps a provider name to {baseURL, loginURL}.
func readProviders(v js.Value) (map[string]providerURLs, error) {
	switch v.Type() {
	case js.TypeUndefined, js.TypeNull:
		return nil, nil
	case js.TypeObject:
	default:
		return nil, errors.New("proxy.providers must be an object")
	}
	out := map[string]providerURLs{}
	keys := js.Global().Get("Object").Call("keys", v)
	for i := range keys.Length() {
		name := keys.Index(i).String()
		entry := v.Get(name)
		if entry.Type() != js.TypeObject {
			return nil, fmt.Errorf("proxy.providers.%s must be an object", name)
		}
		base, err := jshost.StringField(entry, "baseURL")
		if err != nil {
			return nil, fmt.Errorf("proxy.providers.%s: %w", name, err)
		}
		login, err := jshost.StringField(entry, "loginURL")
		if err != nil {
			return nil, fmt.Errorf("proxy.providers.%s: %w", name, err)
		}
		out[name] = providerURLs{baseURL: base, loginURL: login}
	}
	return out, nil
}

// applyHostConfig sets what the host decides in the user config: the base_url and login_url of each provider in
// providers. A provider that the config does not define starts from its native entry, with auth injected, because a
// page holds no credential.
func (o *options) applyHostConfig() error {
	if len(o.providers) == 0 {
		return nil
	}
	providers := maps.Clone(o.user.Providers)
	if providers == nil {
		providers = map[string]config.Provider{}
	}
	for name, u := range o.providers {
		p, ok := providers[name]
		if !ok {
			if p, ok = config.NativeProvider(name); !ok {
				return fmt.Errorf("proxy.providers: %q is not a provider", name)
			}
			if u.baseURL == "" {
				return fmt.Errorf("proxy.providers.%s: baseURL is required, because the native entry would send the request to the public endpoint with no credential", name)
			}
			p.Auth = config.AuthInjected
		}
		p.BaseURL = cmp.Or(u.baseURL, p.BaseURL)
		p.LoginURL = cmp.Or(u.loginURL, p.LoginURL)
		providers[name] = p
	}
	o.user.Providers = providers
	return nil
}

// modelTransport is the transport of the model calls of every provider. The status probe of an injected entry uses it
// too. It follows no redirect: a redirect must not read as a sign-in, and must not carry a request body on.
func (o *options) modelTransport(string) http.RoundTripper {
	return jshost.FetchRT{Headers: o.proxyHeaders, Origin: o.proxyOrigin(), Fetch: o.modelFetch, NoRedirect: true}
}

// hostLimits is what the wasm host cannot do. The login of the connection reads NoLogin from it.
var hostLimits = harness.HostLimits{NoProcess: true, NoLogin: true, NoNetwork: true}

// loginState returns the owner of the injected logins of the connection.
func (o *options) loginState() *injectedauth.Logins {
	if o.logins == nil {
		o.logins = injectedauth.New(injectedauth.Options{Transport: o.modelTransport, Probe: backend.ProbeOf})
	}
	return o.logins
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
func (o *options) runtimes() acp.Runtimes {
	return checkedRuntimes{defaultDir: o.defaultDir, Runtimes: cwdruntimes.New(cwdruntimes.Options{
		Logins: o.loginState(),
		Config: func(cwd string) (*config.Config, error) {
			if o.err != nil {
				return nil, o.err
			}
			cfg, err := config.MergeProject(o.user, "the config of harnessConnect", cwd, o.proxyBase)
			if err != nil {
				return nil, err
			}
			return cfg, nil
		},
		Shared: func(*config.Config) (harness.Store, harness.Owner, error) {
			if o.storeDir == "" {
				return store.NewMemStore(), harness.NewLocalOwner(1), nil
			}
			return store.NewDiskStore(o.storeDir), harness.NewLocalOwner(1), nil
		},
		DefaultDir:     o.defaultDir,
		Version:        "wasm",
		Getenv:         func(k string) string { return o.env[k] },
		Logger:         o.logger,
		ModelTransport: o.modelTransport,
		Tools:          o.tools,
		Host:           hostLimits,
	})}
}

// login is the login of the connection: the injected logins of the config, shown to a client that takes an elicitation in URL mode.
func (o *options) login() acp.Auth {
	return acpauth.New(acpauth.Options{
		Logins: o.loginState(),
		Config: func(cwd string) (*config.Config, error) {
			if cwd == "" {
				cwd = o.defaultDir
			}
			if o.err != nil {
				return nil, o.err
			}
			if cwd == "" {
				return o.user, nil
			}
			return config.MergeProject(o.user, "the config of harnessConnect", cwd, o.proxyBase)
		},
		NoLogin:  hostLimits.NoLogin,
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound },
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
		cfg := acp.Config{Version: "wasm", Logger: opts.logger, AllowedTools: opts.builtinTools, List: opts.defaultDir != "", Auth: opts.login()}
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
