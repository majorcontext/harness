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
	"slices"
	"strings"
	"sync"
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
type Source struct {
	log *slog.Logger
	// specs are the servers that this Source connects. names holds them and the
	// servers of parent, sorted. parent is nil for the Source of the runtime.
	specs  map[string]config.MCPServerSpec
	names  []string
	own    []string
	parent *Source
	// defers is set when a server can defer its tools, which gives the mcp
	// tool its search and select actions.
	defers    bool
	mode      string
	threshold int
	dialing   map[string]chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	start  sync.Once
	ready  chan struct{}
	group  sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	servers map[string]*server
	health  map[string]*health
	// reached holds the servers that the first connect reached. Only they
	// give instructions, so each prompt stays stable.
	reached []string
}

type server struct {
	client *mcp.Client
	tools  []mcp.Tool
}

func (s *server) up() bool { return s != nil && s.client != nil }

func (s *server) resources() bool { return s.up() && s.client.ServerCapabilities().Resources != nil }

// New returns the Source of cfg.MCPServers, or nil when it is empty. It does no I/O. A nil log is slog.Default().
func New(cfg config.Config, log *slog.Logger) *Source {
	if len(cfg.MCPServers) == 0 {
		return nil
	}
	return build(cfg, nil, cfg.MCPServers, log)
}

// NewSession returns the Source of one session: the servers in specs beside the servers of parent, which
// may be nil. It connects only the servers in specs, and Close stops only them. It does no I/O. A name
// in specs must not be a name of parent.
func NewSession(cfg config.Config, parent *Source, specs map[string]config.MCPServerSpec, log *slog.Logger) *Source {
	return build(cfg, parent, specs, log)
}

func build(cfg config.Config, parent *Source, specs map[string]config.MCPServerSpec, log *slog.Logger) *Source {
	s := &Source{log: cmp.Or(log, slog.Default()), specs: specs, own: slices.Sorted(maps.Keys(specs)), parent: parent, mode: cfg.MCPToolLoading,
		threshold: cmp.Or(cfg.MCPToolLoadingThreshold, config.Defaults().MCPToolLoadingThreshold), dialing: map[string]chan struct{}{},
		ready: make(chan struct{}), servers: map[string]*server{}, health: map[string]*health{}}
	s.names = s.own
	if parent != nil {
		s.names = slices.Sorted(slices.Values(slices.Concat(parent.names, s.own)))
		s.defers = parent.defers
	}
	for _, name := range s.own {
		s.dialing[name], s.health[name] = make(chan struct{}, 1), &health{}
		s.defers = s.defers || s.loading(name) != "eager"
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// owns reports whether s connects server itself.
func (s *Source) owns(server string) bool {
	_, ok := s.specs[server]
	return ok
}

func (s *Source) loading(server string) string {
	if !s.owns(server) && s.parent != nil {
		return s.parent.loading(server)
	}
	return cmp.Or(s.specs[server].ToolLoading, s.mode, "eager")
}

// Close closes every connection and stops every stdio server. A nil Source has none.
func (s *Source) Close() {
	if s == nil {
		return
	}
	s.cancel()
	s.start.Do(func() { close(s.ready) })
	s.group.Wait()
	s.mu.Lock()
	s.closed = true
	servers := s.servers
	s.servers = map[string]*server{}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, sv := range servers {
		if sv.up() {
			wg.Go(func() { _ = sv.client.Close() })
		}
	}
	wg.Wait()
}

// wait connects every server once and reports whether that ended before ctx.
func (s *Source) wait(ctx context.Context) bool {
	s.start.Do(func() {
		s.group.Go(func() {
			var wg sync.WaitGroup
			var okMu sync.Mutex
			first := map[string]bool{}
			for _, name := range s.own {
				wg.Go(func() {
					if err := s.connect(s.ctx, name); err != nil {
						s.log.Warn("mcp: server did not connect", "server", name, "err", hide(name, err))
						s.group.Go(func() { s.retry(name) })
						return
					}
					okMu.Lock()
					first[name] = true
					okMu.Unlock()
				})
			}
			wg.Wait()
			s.mu.Lock()
			s.reached = slices.DeleteFunc(slices.Clone(s.own), func(n string) bool { return !first[n] })
			s.mu.Unlock()
			close(s.ready)
		})
	})
	if s.parent != nil && !s.parent.wait(ctx) {
		return false
	}
	select {
	case <-s.ready:
		return true
	case <-ctx.Done():
		return false
	}
}

// connect dials server unless it is connected.
func (s *Source) connect(ctx context.Context, name string) error {
	if !s.owns(name) && s.parent != nil {
		return s.parent.connect(ctx, name)
	}
	lock := s.dialing[name]
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	if s.server(name).up() {
		return nil
	}
	sv, err := dial(ctx, s.specs[name])
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.health[name]
	h.attempts++
	switch {
	case err != nil:
		h.reason = connectReason(err)
		return err
	case s.closed:
		_ = sv.client.Close()
		return errors.New("mcp: closed")
	}
	s.servers[name], h.reason, h.parked = sv, "", false
	return nil
}

func (s *Source) server(name string) *server {
	if !s.owns(name) && s.parent != nil {
		return s.parent.server(name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.servers[name]
}

func dial(ctx context.Context, spec config.MCPServerSpec) (*server, error) {
	var tr mcp.Transport = &mcp.HTTPTransport{Endpoint: spec.URL, Headers: spec.Headers}
	if len(spec.Command) > 0 {
		tr = &mcp.StdioTransport{Command: spec.Command, Env: spec.Env, Dir: spec.Dir}
	}
	c, err := mcp.NewClient(tr, mcp.Options{})
	if err != nil {
		return nil, err
	}
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
	return &server{client: c, tools: tools}, nil
}

// snapshot returns the connected servers, the servers that the first connect reached, and the
// notice text of the servers that are down, including those of the parent.
func (s *Source) snapshot() (servers map[string]*server, reached []string, down string) {
	if s.parent != nil {
		servers, reached, _ = s.parent.snapshot()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if servers == nil {
		servers = map[string]*server{}
	}
	maps.Copy(servers, s.servers)
	return servers, slices.Concat(reached, s.reached), s.unavailable()
}

// Toolset returns the mcp tool, the resource tools when a server serves
// resources, and the tools of each connected server. A tool that defers is
// in Deferred, and the prompt lists it, until history selects or calls it.
func (s *Source) Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, _ string) turn.Toolset {
	if !s.wait(ctx) {
		return turn.Toolset{}
	}
	servers, reached, down := s.snapshot()
	ok := func(name string) bool { return allowed == nil || slices.Contains(allowed, name) }
	var all []remote
	resources := false
	for _, name := range s.names {
		sv := servers[name]
		if !sv.up() {
			continue
		}
		resources = resources || sv.resources()
		for _, t := range sv.tools {
			spec := protocol.ToolSpec{Name: prefix + name + "__" + t.Name, Description: t.Description, InputSchema: t.InputSchema, ReadOnly: readOnlyHint(t.Annotations)}
			if ok(spec.Name) {
				all = append(all, remote{server: name, name: t.Name, client: sv.client, spec: spec})
			}
		}
	}
	loaded := selected(history)
	ctl := control{s: s, all: all, loaded: map[string]bool{}, servers: servers}
	var ts turn.Toolset
	var deferred []remote
	for _, r := range all {
		if l := s.loading(r.server); !loaded[r.spec.Name] && (l == "lazy" || l == "auto" && len(all) > s.threshold) {
			ts.Deferred, deferred = append(ts.Deferred, r), append(deferred, r)
			continue
		}
		ctl.loaded[r.spec.Name] = true
		ts.Tools = append(ts.Tools, r)
	}
	var head []turn.Tool
	if ok(controlName) {
		head = append(head, ctl)
	}
	if resources {
		head = append(head, slices.DeleteFunc([]turn.Tool{lister{s}, reader{s}}, func(t turn.Tool) bool { return !ok(t.Spec().Name) })...)
	}
	ts.Tools = append(head, ts.Tools...)
	ts.Prompt = strings.Join(slices.DeleteFunc([]string{s.instructions(servers, reached, ok), catalog(deferred)}, func(p string) bool { return p == "" }), "\n\n")
	ts.Notices = []turn.Notice{{Kind: noticeKind, Text: down, Cleared: recovered}}
	return ts
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

// remote is one tool of a server.
type remote struct {
	server, name string
	client       *mcp.Client
	spec         protocol.ToolSpec
}

func (r remote) Spec() protocol.ToolSpec { return r.spec }

func (r remote) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &in); err != nil {
			return protocol.ToolResult{}, fmt.Errorf("mcp: invalid arguments for %s: %w", call.Name, err)
		}
	}
	res, err := r.client.CallTool(ctx, r.name, in)
	if err != nil {
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
