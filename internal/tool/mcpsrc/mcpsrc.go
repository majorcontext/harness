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
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/mcp"
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
// of the connected servers to each model call. A server that fails stays
// down until the model asks the mcp tool to connect it.
type Source struct {
	specs map[string]config.MCPServerSpec
	names []string
	// defers is set when a server can defer its tools, which gives the mcp
	// tool its search and select actions.
	defers    bool
	mode      string
	threshold int
	dialing   map[string]*sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	start  sync.Once
	ready  chan struct{}
	group  sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	servers map[string]*server
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

// New returns the Source of cfg.MCPServers, or nil when it is empty. It does no I/O.
func New(cfg config.Config) *Source {
	if len(cfg.MCPServers) == 0 {
		return nil
	}
	s := &Source{specs: cfg.MCPServers, names: slices.Sorted(maps.Keys(cfg.MCPServers)), mode: cfg.MCPToolLoading,
		threshold: cmp.Or(cfg.MCPToolLoadingThreshold, config.Defaults().MCPToolLoadingThreshold), dialing: map[string]*sync.Mutex{},
		ready: make(chan struct{}), servers: map[string]*server{}}
	for _, name := range s.names {
		s.dialing[name] = &sync.Mutex{}
		s.defers = s.defers || s.loading(name) != "eager"
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

func (s *Source) loading(server string) string {
	return cmp.Or(s.specs[server].ToolLoading, s.mode, "eager")
}

// Close closes every connection and stops every stdio server.
func (s *Source) Close() {
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
			for _, name := range s.names {
				wg.Go(func() {
					if err := s.connect(s.ctx, name); err != nil {
						slog.Warn("mcp: server did not connect", "server", name, "err", hide(name, err))
					}
				})
			}
			wg.Wait()
			s.mu.Lock()
			s.reached = slices.DeleteFunc(slices.Clone(s.names), func(n string) bool { return !s.servers[n].up() })
			s.mu.Unlock()
			close(s.ready)
		})
	})
	select {
	case <-s.ready:
		return true
	case <-ctx.Done():
		return false
	}
}

// connect dials server unless it is connected.
func (s *Source) connect(ctx context.Context, name string) error {
	lock := s.dialing[name]
	lock.Lock()
	defer lock.Unlock()
	if s.server(name).up() {
		return nil
	}
	sv, err := dial(ctx, s.specs[name])
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = sv.client.Close()
		return errors.New("mcp: closed")
	}
	s.servers[name] = sv
	return nil
}

func (s *Source) server(name string) *server {
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

// Toolset returns the mcp tool, the resource tools when a server serves
// resources, and the tools of each connected server. A tool that defers is
// in Deferred, and the prompt lists it, until history selects or calls it.
func (s *Source) Toolset(ctx context.Context, history []eventlog.Message, allowed []string, _ string) turn.Toolset {
	if !s.wait(ctx) {
		return turn.Toolset{}
	}
	s.mu.Lock()
	servers, reached := maps.Clone(s.servers), s.reached
	s.mu.Unlock()
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
			spec := protocol.ToolSpec{Name: prefix + name + "__" + t.Name, Description: t.Description, InputSchema: t.InputSchema}
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
	ts.Prompt = strings.Trim(s.instructions(servers, reached, ok)+"\n\n"+catalog(deferred), "\n")
	return ts
}

// selected returns the tool names that history selects with the mcp tool
// or calls. The log holds them, so a replay loads the same tools.
func selected(history []eventlog.Message) map[string]bool {
	out := map[string]bool{}
	for _, m := range history {
		for _, p := range m.Parts {
			var in args
			switch {
			case p.Type != eventlog.PartToolCall:
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
	return protocol.ToolResult{Text: text(res.Content), IsError: res.IsError}, nil
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

// text renders content as text. The result carries no binary data, so a
// binary item becomes one line that names its size and type.
func text(content []mcp.Content) string {
	var lines []string
	for _, c := range content {
		switch {
		case c.Type == mcp.ContentTypeImage, c.Type == mcp.ContentTypeAudio:
			lines = append(lines, binary(c.Type+" content", c.Data, c.MimeType))
		case c.Type == mcp.ContentTypeResourceLink:
			lines = append(lines, fmt.Sprintf("resource: %s (%s)", c.URI, c.Name))
		case c.Type == mcp.ContentTypeResource && c.Resource != nil && c.Resource.Blob != "":
			lines = append(lines, binary("binary resource: "+c.Resource.URI, c.Resource.Blob, c.Resource.MimeType))
		case c.Type == mcp.ContentTypeResource && c.Resource != nil:
			lines = append(lines, c.Resource.Text)
		case c.Text != "":
			lines = append(lines, c.Text)
		}
	}
	return strings.Join(lines, "\n")
}

func binary(what, data, mime string) string {
	mime = cmp.Or(mime, "application/octet-stream")
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	if err != nil {
		return fmt.Sprintf("[%s, malformed base64, %s]", what, mime)
	}
	return fmt.Sprintf("[%s, %d bytes, %s]", what, n, mime)
}
