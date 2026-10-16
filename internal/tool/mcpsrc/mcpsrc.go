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
// The servers of a Source are a live set: SetServers adds, replaces, and
// removes them. The tools that the first connect reached are the tools of
// the request prefix of each session. A tool that arrives later is deferred,
// and a tool that a session sent stays in its definitions.
type Source struct {
	log       *slog.Logger
	parent    *Source
	mode      string
	threshold int

	ctx    context.Context
	cancel context.CancelFunc
	start  sync.Once
	ready  chan struct{}
	group  sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	started bool
	entries map[string]*entry
	mem     map[string]*memory
	// reached holds the servers that the first connect reached. Only they
	// give instructions, so each prompt stays stable.
	reached []string
	// refreshing counts the tool lists that a list_changed notification
	// reads again. idle is closed when it is zero.
	refreshing int
	idle       chan struct{}
}

// entry is one connected-or-connecting server. Source.mu guards every field but the immutable ones.
type entry struct {
	name    string
	spec    config.MCPServerSpec
	ctx     context.Context
	cancel  context.CancelFunc
	dialing chan struct{}
	health  health
	sv      *server
	// gen counts the reads of the tool list that a notification started, and applied is the newest one that took effect.
	gen, applied int
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

func (s *server) resources() bool { return s.up() && s.client.ServerCapabilities().Resources != nil }

func (s *server) has(tool string) bool { return s.up() && s.index[tool] }

// New returns the Source of cfg.MCPServers. It does no I/O. A nil log is slog.Default().
func New(cfg config.Config, log *slog.Logger) *Source {
	return build(cfg, nil, cfg.MCPServers, log)
}

// NewSession returns the Source of one session: the servers in specs beside the servers of parent, which
// may be nil. It connects only the servers in specs, and Close stops only them. It does no I/O. A name
// in specs must not be a name of parent.
func NewSession(cfg config.Config, parent *Source, specs map[string]config.MCPServerSpec, log *slog.Logger) *Source {
	return build(cfg, parent, specs, log)
}

func build(cfg config.Config, parent *Source, specs map[string]config.MCPServerSpec, log *slog.Logger) *Source {
	s := &Source{log: cmp.Or(log, slog.Default()), parent: parent, mode: cfg.MCPToolLoading,
		threshold: cmp.Or(cfg.MCPToolLoadingThreshold, config.Defaults().MCPToolLoadingThreshold),
		ready:     make(chan struct{}), entries: map[string]*entry{}, mem: map[string]*memory{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for name, spec := range specs {
		s.add(name, spec)
	}
	return s
}

// add makes the entry of a server. The caller holds s.mu, or owns s alone.
func (s *Source) add(name string, spec config.MCPServerSpec) *entry {
	e := &entry{name: name, spec: cloneSpec(spec), dialing: make(chan struct{}, 1)}
	e.ctx, e.cancel = context.WithCancel(s.ctx)
	s.entries[name] = e
	loading := cmp.Or(spec.ToolLoading, s.mode, "eager")
	switch m := s.mem[name]; {
	case m == nil:
		s.mem[name] = &memory{loading: loading, seen: map[string]mcp.Tool{}, base: map[string]bool{}}
	case len(m.base) == 0 && m.loading != loading:
		c := *m
		c.loading = loading
		s.mem[name] = &c
	}
	return e
}

// Active reports whether s ever had a server.
func (s *Source) Active() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mem) > 0 || s.parent.Active()
}

// find returns the Source that owns server and its entry, or nil for a name that no server has.
func (s *Source) find(name string) (*Source, *entry) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	e := s.entries[name]
	s.mu.Unlock()
	if e != nil {
		return s, e
	}
	return s.parent.find(name)
}

// Close closes every connection and stops every stdio server. A nil Source has none.
func (s *Source) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.start.Do(func() { close(s.ready) })
	s.group.Wait()
	s.mu.Lock()
	var clients []*mcp.Client
	for _, e := range s.entries {
		if e.sv.up() {
			clients = append(clients, e.sv.client)
		}
		e.sv = nil
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Go(func() { _ = c.Close() })
	}
	wg.Wait()
}

// spawn runs f in the group of s unless s is closed.
func (s *Source) spawn(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.group.Go(f)
	}
}

// wait connects every server once and reports whether that ended before ctx.
func (s *Source) wait(ctx context.Context) bool {
	s.start.Do(func() { s.group.Go(s.firstConnect) })
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

// firstConnect connects the servers that s has when it starts, in parallel. The tools that it reaches
// are the eager tools of every session.
func (s *Source) firstConnect() {
	s.mu.Lock()
	s.started = true
	set := slices.Collect(maps.Values(s.entries))
	s.mu.Unlock()
	var wg sync.WaitGroup
	var okMu sync.Mutex
	first := map[string]bool{}
	for _, e := range set {
		wg.Go(func() {
			if _, err := s.connect(s.ctx, e); err != nil {
				s.log.Warn("mcp: server did not connect", "server", e.name, "err", hide(e.name, err))
				s.spawn(func() { s.retry(e) })
				return
			}
			okMu.Lock()
			first[e.name] = true
			okMu.Unlock()
		})
	}
	wg.Wait()
	s.mu.Lock()
	for _, e := range set {
		if first[e.name] && s.entries[e.name] == e && e.sv.up() {
			s.reached = append(s.reached, e.name)
			s.freeze(e)
		}
	}
	slices.Sort(s.reached)
	s.mu.Unlock()
	close(s.ready)
}

// freeze records what the first connect reached of e. The caller holds s.mu.
func (s *Source) freeze(e *entry) {
	m := *s.mem[e.name]
	m.base = map[string]bool{}
	for _, t := range e.sv.tools {
		m.base[t.Name] = true
	}
	m.inst, m.resources = e.sv.client.Instructions(), e.sv.resources()
	s.mem[e.name] = &m
}

// remember adds the tools of sv to what the runtime read of the server. The caller holds s.mu.
func (s *Source) remember(name string, sv *server) {
	m := *s.mem[name]
	m.up, m.seen = true, maps.Clone(m.seen)
	for _, t := range sv.tools {
		if _, ok := m.seen[t.Name]; !ok {
			m.seen[t.Name] = t
		}
	}
	s.mem[name] = &m
}

// connect dials e unless it is connected, and reports whether it dialed. It ends when ctx or the entry ends.
func (s *Source) connect(ctx context.Context, e *entry) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(e.ctx, cancel)()
	select {
	case e.dialing <- struct{}{}:
		defer func() { <-e.dialing }()
	case <-ctx.Done():
		return false, context.Cause(ctx)
	}
	return s.dialHeld(ctx, e)
}

// dialHeld is connect for a caller that holds e.dialing.
func (s *Source) dialHeld(ctx context.Context, e *entry) (bool, error) {
	if s.connected(e) {
		return false, nil
	}
	sv, err := dial(ctx, e.spec, func(c *mcp.Client) { s.listChanged(e, c) })
	s.mu.Lock()
	defer s.mu.Unlock()
	e.health.attempts++
	switch {
	case err != nil:
		e.health.reason = connectReason(err)
		return true, err
	case s.closed || s.entries[e.name] != e:
		_ = sv.client.Close()
		return true, errors.New("mcp: closed")
	}
	e.sv, e.health.reason, e.health.parked = sv, "", false
	s.remember(e.name, sv)
	return true, nil
}

func (s *Source) connected(e *entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return e.sv.up()
}

func dial(ctx context.Context, spec config.MCPServerSpec, changed func(*mcp.Client)) (*server, error) {
	var tr mcp.Transport = &mcp.HTTPTransport{Endpoint: spec.URL, Headers: spec.Headers}
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

// view is what one model call reads of the servers.
type view struct {
	// names are the servers that exist now, sorted.
	names  []string
	live   map[string]*server
	life   map[string]context.Context
	health map[string]health
	// mem holds every server name that the runtime read, also a removed one.
	mem     map[string]*memory
	reached []string
}

// view returns the servers of s and of its parent.
func (s *Source) view() view {
	v := view{live: map[string]*server{}, life: map[string]context.Context{}, health: map[string]health{}, mem: map[string]*memory{}}
	if s.parent != nil {
		v = s.parent.view()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, e := range s.entries {
		v.names = append(v.names, name)
		v.health[name] = e.health
		if e.sv.up() {
			v.live[name], v.life[name] = e.sv, e.ctx
		}
	}
	slices.Sort(v.names)
	maps.Copy(v.mem, s.mem)
	v.reached = slices.Concat(v.reached, s.reached)
	return v
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
	ctl := control{s: s, loaded: map[string]bool{}, v: v}
	var ts turn.Toolset
	var listed []remote
	for _, name := range slices.Sorted(maps.Keys(v.mem)) {
		m := v.mem[name]
		deferred := m.loading == "lazy" || m.loading == "auto" && autoDefer
		for _, r := range v.remotes(name, ok) {
			base := m.base[r.name]
			if r.client != nil {
				ctl.all = append(ctl.all, r)
			}
			switch {
			case loaded[r.spec.Name] || base && !deferred:
				ctl.loaded[r.spec.Name] = true
				ts.Tools = append(ts.Tools, r)
			case r.client != nil:
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
		r := remote{server: name, name: tn, spec: spec}
		if sv.has(tn) {
			r.client, r.life = sv.client, v.life[name]
		}
		out = append(out, r)
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

// remote is one tool of a server. It has no client when its server is gone or does not list it, and a
// call then reads no such tool available. life ends when the server goes.
type remote struct {
	server, name string
	client       *mcp.Client
	life         context.Context
	spec         protocol.ToolSpec
}

func (r remote) Spec() protocol.ToolSpec { return r.spec }

func (r remote) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	if r.client == nil {
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
	defer context.AfterFunc(r.life, cancel)()
	res, err := r.client.CallTool(ctx, r.name, in)
	if err != nil {
		if r.life.Err() != nil {
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
