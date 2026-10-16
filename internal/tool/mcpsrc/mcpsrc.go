// Package mcpsrc gives the tools of the configured MCP servers to each
// model call.
package mcpsrc

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const (
	prefix         = "mcp__"
	controlName    = "mcp"
	listName       = "list_mcp_resources"
	readName       = "read_mcp_resource"
	connectTimeout = 15 * time.Second
)

// Reserved reports whether name belongs to the tools of a Source.
func Reserved(name string) bool {
	return name == controlName || name == listName || name == readName || strings.HasPrefix(name, prefix)
}

// Source connects every configured server on first use and gives the tools
// of the connected servers to each model call. A server that fails retries
// in the background, and a notice in the conversation names it while it is
// down. After maxRetries failed retries it stays down until the model asks
// the mcp tool to connect it.
//
// The servers of a Source are a live set: Set adds, replaces, and removes
// them. One actor goroutine owns every server entry (see actor.go). Every
// other goroutine sends it a message or reads the snapshot that it publishes
// after each change. The tools that the first connect reached are the tools
// of the request prefix of each session. A tool that arrives later is
// deferred, and a tool that a session sent stays in its definitions.
type Source struct {
	log       *slog.Logger
	parent    *Source
	mode      string
	threshold int
	// httpClient is the client of the HTTP servers; nil is the default client. A test sets it.
	httpClient *http.Client

	ctx       context.Context
	cancel    context.CancelFunc
	inbox     chan msg
	quit      chan struct{}
	done      chan struct{}
	ready     chan struct{}
	snap      atomic.Pointer[snapshot]
	workers   sync.WaitGroup
	startOnce sync.Once
	closeOnce sync.Once
}

// memory is what the runtime keeps of a server name for the whole process, so a session that sent a tool
// finds its definition again after the server goes. It is replaced, never changed in place.
type memory struct {
	loading string
	// seen holds the first definition that the runtime read of each tool.
	seen map[string]mcp.Tool
	// base holds the tools that the first connect reached. They are the tools that every session sends.
	base      map[string]bool
	inst      string
	resources bool
	// up is true once the server connected.
	up bool
}

type server struct {
	client *mcp.Client
	tools  []mcp.Tool
	index  map[string]bool
}

func newServer(c *mcp.Client, tools []mcp.Tool) *server {
	sv := &server{client: c, tools: tools, index: make(map[string]bool, len(tools))}
	for _, t := range tools {
		sv.index[t.Name] = true
	}
	return sv
}

func (s *server) up() bool { return s != nil && s.client != nil }

// clientOf returns the client of s, or nil.
func (s *server) clientOf() *mcp.Client {
	if s == nil {
		return nil
	}
	return s.client
}

func (s *server) resources() bool { return s.up() && s.client.ServerCapabilities().Resources != nil }

func (s *server) has(tool string) bool { return s.up() && s.index[tool] }

// New returns the Source of cfg.MCPServers. It dials nothing. A nil log is slog.Default(). Close ends its goroutine.
func New(cfg config.Config, log *slog.Logger) *Source {
	return build(cfg, nil, cfg.MCPServers, log)
}

// NewSession returns the Source of one session: the servers in specs beside the servers of parent, which
// may be nil. It connects only the servers in specs, and Close stops only them. It dials nothing. A name
// in specs must not be a name of parent.
func NewSession(cfg config.Config, parent *Source, specs map[string]config.MCPServerSpec, log *slog.Logger) *Source {
	return build(cfg, parent, specs, log)
}

func build(cfg config.Config, parent *Source, specs map[string]config.MCPServerSpec, log *slog.Logger) *Source {
	s := &Source{log: cmp.Or(log, slog.Default()), parent: parent, mode: cfg.MCPToolLoading,
		threshold: cmp.Or(cfg.MCPToolLoadingThreshold, config.Defaults().MCPToolLoadingThreshold),
		inbox:     make(chan msg), quit: make(chan struct{}), done: make(chan struct{}), ready: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	a := newActor(s)
	for name, spec := range specs {
		a.add(name, spec)
	}
	a.publish()
	go a.run()
	return s
}

// dial connects to the server of spec and reads its tools. changed receives the client when the server says that
// its tool list changed.
func dial(ctx context.Context, spec config.MCPServerSpec, hc *http.Client, changed func(*mcp.Client)) (*server, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	var tr mcp.Transport = &mcp.HTTPTransport{Endpoint: spec.URL, Headers: spec.Headers, Client: hc}
	if len(spec.Command) > 0 {
		tr = &mcp.StdioTransport{Command: spec.Command, Env: spec.Env, Dir: spec.Dir}
	}
	var client atomic.Pointer[mcp.Client]
	c, err := mcp.NewClient(tr, mcp.Options{OnNotification: func(method string, _ json.RawMessage) {
		if cl := client.Load(); method == mcp.NotificationToolsListChanged && cl != nil {
			if t := cl.ServerCapabilities().Tools; t != nil && t.ListChanged {
				changed(cl)
			}
		}
	}})
	if err != nil {
		return nil, err
	}
	client.Store(c)
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(time.Duration(spec.ConnectTimeoutS)*time.Second, connectTimeout))
	defer cancel()
	init, err := c.Initialize(ctx)
	var tools []mcp.Tool
	if err == nil && init.Capabilities.Tools != nil {
		tools, err = c.ListAllTools(ctx)
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	slices.SortFunc(tools, func(a, b mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	return newServer(c, tools), nil
}

// Toolset returns the mcp tool, the resource tools when a server that the
// first connect reached serves resources, and the tools of each server. A
// tool that defers is in Deferred, and the prompt lists it when the first
// connect reached it, until history selects or calls it. A tool that arrives
// later is deferred and a notice lists it.
func (s *Source) Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, _ string) turn.Toolset {
	if !s.wait(ctx) || !s.settle(ctx) {
		return turn.Toolset{}
	}
	v := s.view()
	if len(v.mem) == 0 {
		return turn.Toolset{}
	}
	ok := func(name string) bool { return allowed == nil || slices.Contains(allowed, name) }
	autoDefer := v.baseCount(ok) > s.threshold
	loaded := selected(history)
	ctl := control{s: s, loaded: map[string]bool{}, v: v, ok: ok}
	var ts turn.Toolset
	var listed []remote
	for _, name := range slices.Sorted(maps.Keys(v.mem)) {
		m := v.mem[name]
		deferred := m.loading == "lazy" || m.loading == "auto" && autoDefer
		for _, r := range v.remotes(name, ok) {
			base := m.base[r.name]
			if r.listed {
				ctl.all = append(ctl.all, r)
			}
			switch {
			case loaded[r.spec.Name] || base && !deferred:
				ctl.loaded[r.spec.Name] = true
				ts.Tools = append(ts.Tools, r)
			case r.listed:
				ts.Deferred = append(ts.Deferred, r)
				if base {
					listed = append(listed, r)
				}
			case base:
				listed = append(listed, r)
			}
		}
	}
	var head []turn.Tool
	if ok(controlName) {
		head = append(head, ctl)
	}
	if slices.ContainsFunc(v.reached, func(n string) bool { return v.mem[n].resources }) {
		head = append(head, slices.DeleteFunc([]turn.Tool{lister{s}, reader{s}}, func(t turn.Tool) bool { return !ok(t.Spec().Name) })...)
	}
	ts.Tools = append(head, ts.Tools...)
	ts.Prompt = strings.Join(slices.DeleteFunc([]string{v.instructions(ok), catalog(listed)}, func(p string) bool { return p == "" }), "\n\n")
	ts.Notices = []turn.Notice{{Kind: noticeKind, Text: v.unavailable(), Cleared: recovered}, {Kind: addedKind, Text: v.added(ok)}}
	return ts
}

// liveAll returns the tools that their servers list now and ok allows.
func (v view) liveAll(ok func(string) bool) []remote {
	var out []remote
	for _, name := range slices.Sorted(maps.Keys(v.mem)) {
		for _, r := range v.remotes(name, ok) {
			if r.listed {
				out = append(out, r)
			}
		}
	}
	return out
}

// baseCount counts the tools that the first connect reached and ok allows.
func (v view) baseCount(ok func(string) bool) int {
	n := 0
	for name, m := range v.mem {
		for tn := range m.base {
			if ok(prefix + name + "__" + tn) {
				n++
			}
		}
	}
	return n
}

// remotes returns the tools of server name that ok allows, sorted. A tool that its server does not list now
// has no client.
func (v view) remotes(name string, ok func(string) bool) []remote {
	var out []remote
	sv, m := v.live[name], v.mem[name]
	for _, tn := range slices.Sorted(maps.Keys(m.seen)) {
		t := m.seen[tn]
		spec := protocol.ToolSpec{Name: prefix + name + "__" + tn, Description: t.Description, InputSchema: t.InputSchema, ReadOnly: readOnlyHint(t.Annotations)}
		if !ok(spec.Name) {
			continue
		}
		out = append(out, remote{src: v.src, server: name, name: tn, spec: spec, listed: sv.has(tn)})
	}
	return out
}

// selected returns the tool names that history selects with the mcp tool
// or calls. The log holds them, so a replay loads the same tools.
func selected(history []protocol.LogMessage) map[string]bool {
	out := map[string]bool{}
	for _, m := range history {
		for _, p := range m.Parts {
			var in args
			switch {
			case p.Type != protocol.LogPartToolCall:
			case strings.HasPrefix(p.Name, prefix):
				out[p.Name] = true
			case p.Name == controlName && json.Unmarshal(p.Arguments, &in) == nil && in.Action == "select":
				for _, n := range in.Tools {
					out[n] = true
				}
			}
		}
	}
	return out
}

// remote is one tool of a server. A call reads the current snapshot of its server first, and answers no such tool
// available when the server is gone or does not list the tool now, also when the tool was listed when the model
// call began.
type remote struct {
	src          *Source
	server, name string
	spec         protocol.ToolSpec
	listed       bool
}

func (r remote) Spec() protocol.ToolSpec { return r.spec }

func (r remote) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	sv, life := r.src.serverNow(r.server)
	if !sv.has(r.name) {
		return protocol.ToolResult{Text: "no such tool available: " + call.Name, IsError: true}, nil
	}
	var in any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &in); err != nil {
			return protocol.ToolResult{}, fmt.Errorf("mcp: invalid arguments for %s: %w", call.Name, err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(life, cancel)()
	res, err := sv.client.CallTool(ctx, r.name, in)
	if err != nil {
		if life.Err() != nil {
			return protocol.ToolResult{}, fmt.Errorf("mcp: server %q: call failed: server removed", r.server)
		}
		return protocol.ToolResult{}, hide(r.server, err)
	}
	text, blobs := render(res.Content)
	return protocol.ToolResult{Text: text, IsError: res.IsError, Blobs: blobs}, nil
}

// hide replaces every error but a server's own RPC error with a reason. A
// transport error can name the endpoint URL, and an HTTP error the response
// body, and a secret in either.
func hide(server string, err error) error {
	var rpc *mcp.RPCError
	if errors.As(err, &rpc) {
		return err
	}
	return fmt.Errorf("mcp: server %q: call failed: %s", server, reason(err))
}

// reason classifies a connect or transport error without its text.
func reason(err error) string {
	var oe *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &oe):
		return "connection failed"
	}
	return "request failed"
}

// connectReason classifies a failed connect without its text. An HTTP
// status error has no typed code, so it reads as a failed initialize.
func connectReason(err error) string {
	var oe *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "initialize timed out"
	case errors.Is(err, context.Canceled):
		return "initialize cancelled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &oe):
		return "connection failed"
	}
	return "initialize failed"
}

// render splits content into text and blobs. Image and audio content and a
// resource blob become blobs for the model. A payload that is not base64, or
// has no bytes, becomes one line that names its type.
func render(content []mcp.Content) (string, []protocol.Blob) {
	var lines []string
	var blobs []protocol.Blob
	add := func(what, data, mime string) {
		if b, err := base64.StdEncoding.DecodeString(data); err == nil && len(b) > 0 {
			blobs = append(blobs, protocol.Blob{MediaType: mime, Data: b})
			return
		}
		lines = append(lines, binary(what, data, mime))
	}
	for _, c := range content {
		switch {
		case c.Type == mcp.ContentTypeImage, c.Type == mcp.ContentTypeAudio:
			add(c.Type+" content", c.Data, c.MimeType)
		case c.Type == mcp.ContentTypeResourceLink:
			lines = append(lines, fmt.Sprintf("resource: %s (%s)", c.URI, c.Name))
		case c.Type == mcp.ContentTypeResource && c.Resource != nil && c.Resource.Text != "":
			lines = append(lines, c.Resource.Text)
		case c.Type == mcp.ContentTypeResource && c.Resource != nil && c.Resource.Blob != "":
			add("binary resource: "+c.Resource.URI, c.Resource.Blob, c.Resource.MimeType)
		case c.Type == mcp.ContentTypeResource:
		case c.Text != "":
			lines = append(lines, c.Text)
		}
	}
	return strings.Join(lines, "\n"), blobs
}

func binary(what, data, mime string) string {
	mime = cmp.Or(mime, "application/octet-stream")
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	if err != nil {
		return fmt.Sprintf("[%s, malformed base64, %s]", what, mime)
	}
	return fmt.Sprintf("[%s, %d bytes, %s]", what, n, mime)
}

// readOnlyHint reports whether the annotations of an MCP tool set readOnlyHint.
func readOnlyHint(annotations json.RawMessage) bool {
	var a struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	}
	return json.Unmarshal(annotations, &a) == nil && a.ReadOnlyHint
}
