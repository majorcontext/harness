// MCP (Model Context Protocol) client integration: connecting to configured
// MCP servers, registering their tools on a session's tool list, and
// routing both engine-driven tool calls and plugin-initiated
// client/mcp.call requests through the same connected clients.
//
// MCPServerConfig/MCPManager mirror the plugin Host's shape deliberately:
// exactly like plugin.Host, an *MCPManager is built once per process (see
// cmd/harness) and shared across every session via Config.MCP, not
// reconnected per session. "When a session starts, connect to each
// configured MCP server" (see the config package doc) is therefore true in
// the same lazy sense NewSession's own doc comment promises for provider
// auth and plugin spawns: nothing touches the network until first use —
// here, a session's first Prompt calling Tools() or CallTool() — and each
// server's FIRST connect attempt happens then, bounded by its own
// ConnectTimeout.
//
// A server that fails its first connect (dial error, non-2xx, malformed
// handshake) or fails tools/list is logged and skipped for THAT call: this
// is fail-open, the same philosophy as a crashed plugin (see
// plugin/PROTOCOL.md) — one bad server must never prevent a session from
// starting or take down an otherwise-healthy set of tools. It is not
// dropped forever, though: a failed server gets a detached, indefinite
// background retry on a capped exponential backoff (see mcpRetryDelay),
// because a same-second cluster of cold-start timeouts across many remote
// servers is exactly the kind of transient condition that clears on its
// own — see docs/plans/2026-07-20-mcp-init-resilience.md for the incident
// this generalizes from. A HEALTHY server, by contrast, is never
// re-probed: once connected it is done for the process's life, exactly
// like the old exactly-once behavior this replaces (see
// TestMCPManagerHealthyServerNeverReprobedWhileSiblingRetries). Tools()/CallTool()/
// CallServerTool always read live, mu-guarded state, so a server that
// recovers mid-session starts contributing tools on the very next call —
// no new session, no explicit trigger (engine/engine.go's per-request
// toolDefs assembly already re-reads Tools() every turn).
package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// mcpToolPrefix namespaces every MCP-provided tool name: mcp__<server>__<tool>,
// the Claude Code convention.
const mcpToolPrefix = "mcp__"

// mcpToolName builds the namespaced tool name for a server+tool pair.
func mcpToolName(server, tool string) string {
	return mcpToolPrefix + server + "__" + tool
}

// isMCPToolName reports whether name looks like a namespaced MCP tool name.
func isMCPToolName(name string) bool {
	return strings.HasPrefix(name, mcpToolPrefix)
}

// defaultMCPConnectTimeout bounds Initialize+ListAllTools for one server
// when MCPServerConfig.ConnectTimeout is unset. It exists on top of
// mcp.Client's own per-request timeout (30s default) as an outer,
// per-server bound on the whole connect step (initialize + however many
// tools/list pages it takes).
const defaultMCPConnectTimeout = 15 * time.Second

// MCPServerConfig configures one MCP server a session's MCPManager connects
// to. Exactly one of Command (a stdio server) or URL (a Streamable HTTP
// server) must be set — see config.MCPServerSpec, which this mirrors
// one-for-one for the cmd/harness wiring that builds it from config.
type MCPServerConfig struct {
	// Command is the argv of a stdio MCP server process.
	Command []string
	// Env is appended to the harness environment when the stdio server is
	// spawned.
	Env []string
	// Dir is the stdio server process's working directory.
	Dir string
	// URL is a Streamable HTTP MCP server's endpoint.
	URL string
	// Headers are static headers sent on every request to a Streamable
	// HTTP server.
	Headers map[string]string
	// ConnectTimeout bounds Initialize+ListAllTools for this server; <= 0
	// defaults to defaultMCPConnectTimeout.
	ConnectTimeout time.Duration
}

// MCPRegistry is the slice of MCP client integration a Session needs: the
// current namespaced tool list, routing for a namespaced tool call, and
// routing for a plugin-initiated client/mcp.call naming a server and tool
// directly. *MCPManager is the production implementation, built once per
// process (like plugin.Host) and shared across every session via
// Config.MCP; tests use fakes. A nil Config.MCP disables MCP entirely —
// toolDefs contributes nothing and no tool name is recognized as an MCP
// tool.
type MCPRegistry interface {
	// Tools returns the current namespaced tool defs (mcp__<server>__<tool>),
	// connecting to any not-yet-connected configured server on first call.
	// A server that fails to connect or list tools is skipped, never
	// causing this call itself to fail.
	Tools(ctx context.Context) []provider.ToolDef
	// CallTool routes a namespaced tool call (as returned by Tools) to the
	// underlying server. isErr distinguishes a tool-level failure
	// (CallToolResult.IsError) from err, a protocol/connectivity failure.
	CallTool(ctx context.Context, name string, args json.RawMessage) (out message.Parts, isErr bool, err error)
	// CallServerTool routes an explicit server+tool call (unnamespaced),
	// for plugin-initiated client/mcp.call requests (see
	// plugin.ClientAPI.MCPCall and Session.MCPCall).
	CallServerTool(ctx context.Context, server, tool string, args json.RawMessage) (out message.Parts, isErr bool, err error)
}

// mcpToolBinding records how a namespaced tool name maps back to its
// server and remote (unnamespaced) tool name.
type mcpToolBinding struct {
	server string
	remote string
	def    provider.ToolDef
}

// mcpServerEntry is one server's live connection state, guarded by
// MCPManager.mu. Every configured server gets exactly one entry, created
// the moment ensureConnected's first (and only) parallel attempt batch
// commits — after that, entry only ever transitions failed -> connected
// (via a background retryServer goroutine), never the other direction:
// there is no liveness monitoring of an already-connected server, so
// Connected is a one-way latch, matching the "healthy server initialized
// exactly once, never re-probed" invariant.
type mcpServerEntry struct {
	Connected bool
	client    *mcp.Client
	tools     map[string]mcpToolBinding // this server's own bindings, namespaced name -> binding
	// Attempts and LastErr are updated after every attempt (the first one
	// and each background retry) — exported-cased for a future Task 2
	// status surface to read directly, though nothing in this package does
	// yet.
	Attempts int
	LastErr  error
}

// MCPManager is the production MCPRegistry: it owns one mcp.Client per
// configured server that has connected. A server's first connect attempt
// happens lazily, on the first call to Tools/CallTool/CallServerTool,
// bounded by its ConnectTimeout; a server that fails gets an indefinite
// background retry (see retryServer) instead of being dropped for the
// manager's whole life. Safe for concurrent use.
type MCPManager struct {
	servers map[string]MCPServerConfig

	// connectOnce gates the single, one-time, all-servers-in-parallel FIRST
	// attempt batch (see ensureConnected) — never the background retries,
	// which run independently per failed server (see retryServer).
	connectOnce sync.Once

	mu           sync.RWMutex
	state        map[string]*mcpServerEntry // one entry per configured server, populated once ensureConnected's first batch has run
	tools        map[string]mcpToolBinding  // merged view across every currently-connected server; namespaced name -> binding
	toolsOrdered []provider.ToolDef

	// retryCtx/retryCancel bound every background retry goroutine's
	// lifetime — cancelled by Close, independent of any caller's ctx
	// (retries are not request-scoped work). retryWG lets Close wait for
	// every retry goroutine to observe the cancellation and exit before
	// Close closes the connected clients, so a retry that wins the race
	// and connects concurrently with Close is never left un-accounted-for
	// (see Close's doc comment).
	retryCtx    context.Context
	retryCancel context.CancelFunc
	retryWG     sync.WaitGroup
}

// NewMCPManager builds an MCPManager for the given servers. Nothing touches
// the network here — connecting happens lazily on the first call to Tools
// or CallTool/CallServerTool (see the package doc). Building the
// cancellable retry context is pure in-memory bookkeeping, not a startup
// budget violation.
func NewMCPManager(servers map[string]MCPServerConfig) *MCPManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &MCPManager{servers: servers, retryCtx: ctx, retryCancel: cancel}
}

// ensureConnected runs the FIRST connect attempt for every configured
// server exactly once, concurrently, each bounded by its own
// ConnectTimeout. A server whose first attempt fails is logged and
// contributes no client/tools for now; it never causes this (or any other
// server's) first attempt to fail, and — unlike the old exactly-once
// behavior — it is not abandoned: a background retryServer goroutine is
// spawned for it before this method returns, so it keeps trying
// indefinitely on a capped backoff (see retryServer, mcpRetryDelay) until
// it connects or the manager is closed.
//
// The connect step is deliberately detached from ctx (the first caller to
// trigger it, via context.WithoutCancel) rather than run under it: this is
// a one-time (sync.Once) operation shared by every future caller, not a
// request-scoped one, so it must not inherit a request-scoped deadline or
// cancellation. In serve mode ctx is a per-request context — without
// detaching, a request that happens to trigger the first connect being
// aborted/disconnected mid-connect would cancel every server's connect,
// which would be logged-and-skipped and (this part is unchanged from the
// old bug this originally fixed) never retried on THIS SAME batch: one
// transient cancellation would strip a server's tools for its first
// attempt. Each server's own ConnectTimeout still bounds how long this can
// take; the background retries below run under the manager's own
// retryCtx, not connectCtx, so they are unaffected by any caller's
// cancellation either way.
func (m *MCPManager) ensureConnected(ctx context.Context) {
	m.connectOnce.Do(func() {
		connectCtx := context.WithoutCancel(ctx)

		names := make([]string, 0, len(m.servers))
		for name := range m.servers {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic registration/log order

		type attemptResult struct {
			name   string
			client *mcp.Client
			tools  []mcp.Tool
			err    error
		}
		results := make(chan attemptResult, len(names))

		var wg sync.WaitGroup
		for _, name := range names {
			name, spec := name, m.servers[name]
			wg.Add(1)
			go func() {
				defer wg.Done()
				client, toolList, err := mcpConnectFunc(connectCtx, name, spec)
				results <- attemptResult{name: name, client: client, tools: toolList, err: err}
			}()
		}
		wg.Wait()
		close(results)

		m.mu.Lock()
		m.state = make(map[string]*mcpServerEntry, len(names))
		var failed []string
		for r := range results {
			if r.err != nil {
				log.Printf("engine: mcp server %q: %v (retrying in the background)", r.name, r.err)
				m.state[r.name] = &mcpServerEntry{Attempts: 1, LastErr: r.err}
				failed = append(failed, r.name)
				continue
			}
			m.state[r.name] = &mcpServerEntry{Connected: true, client: r.client, tools: mcpBindTools(r.name, r.tools), Attempts: 1}
		}
		m.rebuildToolsLocked()
		sort.Strings(failed) // deterministic spawn order, matching names
		for _, name := range failed {
			m.retryWG.Add(1)
			go m.retryServer(name, 1)
		}
		m.mu.Unlock()
	})
}

// mcpConnectFunc is the seam ensureConnected/retryServer call to perform
// one connect attempt. Production always uses connectMCPServer; tests that
// need to assert a backoff SCHEDULE inside a testing/synctest bubble
// substitute a network-free fake here instead (see AGENTS.md: real network
// I/O does not behave deterministically inside a synctest bubble), so the
// bubble contains no real dials and its fake clock can fast-forward
// through the whole retry schedule.
var mcpConnectFunc = connectMCPServer

// mcpTestRetryCommitted, when non-nil, is called (outside m.mu, strictly
// after the commit's Unlock so the committed state is already visible to
// any goroutine that acquires the lock afterward) every time retryServer
// commits an outcome for one server — success (connected == true) or a
// further failure (connected == false). Nil in production; a test-only
// synchronization hook for the one test (real-network, real backoff, see
// TestMCPManagerCallServerToolRetryingThenRecovers) that cannot use
// synctest.Wait() because it deliberately exercises a real mcp.Client
// round trip end to end — it lets that test block on an actual commit
// event instead of racing the wire or sleeping.
var mcpTestRetryCommitted func(server string, connected bool)

// mcpBindTools converts one server's listed tools into its namespaced
// tool-binding map (mcp__<server>__<tool> -> binding).
func mcpBindTools(server string, toolList []mcp.Tool) map[string]mcpToolBinding {
	tools := make(map[string]mcpToolBinding, len(toolList))
	for _, t := range toolList {
		full := mcpToolName(server, t.Name)
		tools[full] = mcpToolBinding{
			server: server,
			remote: t.Name,
			def: provider.ToolDef{
				Name:        full,
				Description: t.Description,
				InputSchema: t.InputSchema,
			},
		}
	}
	return tools
}

// rebuildToolsLocked recomputes the merged tools/toolsOrdered view from
// every currently-connected server's own tool bindings. Must be called
// with m.mu held for writing. Deterministic: servers, then each server's
// own tools, are visited in sorted-name order — matching the old
// single-pass merge's ordering exactly.
func (m *MCPManager) rebuildToolsLocked() {
	names := make([]string, 0, len(m.state))
	for name, e := range m.state {
		if e.Connected {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	tools := make(map[string]mcpToolBinding)
	ordered := make([]provider.ToolDef, 0)
	for _, name := range names {
		entry := m.state[name]
		toolNames := make([]string, 0, len(entry.tools))
		for tn := range entry.tools {
			toolNames = append(toolNames, tn)
		}
		sort.Strings(toolNames)
		for _, tn := range toolNames {
			b := entry.tools[tn]
			tools[tn] = b
			ordered = append(ordered, b.def)
		}
	}
	m.tools = tools
	m.toolsOrdered = ordered
}

// mcpRetryBackoffBase, mcpRetryBackoffMultiplier, and mcpRetryBackoffCap
// define the capped exponential schedule a failed server's background
// retry waits between attempts: ~1s after the first failure, doubling each
// subsequent failure, capped at 5 minutes — indefinitely, there is no
// "given up" state (see the package doc's incident reference). Mirrors
// goal.go's goalRetryableDelay shape one-for-one, just with MCP's own
// base/cap.
const (
	mcpRetryBackoffBase       = 1 * time.Second
	mcpRetryBackoffMultiplier = 2
	mcpRetryBackoffCap        = 5 * time.Minute
)

// mcpRetryDelay returns the base (pre-jitter) backoff for the given
// 1-indexed attempt that just failed, doubling each time up to
// mcpRetryBackoffCap.
func mcpRetryDelay(attempt int) time.Duration {
	d := mcpRetryBackoffBase
	for i := 1; i < attempt; i++ {
		if d >= mcpRetryBackoffCap {
			return mcpRetryBackoffCap
		}
		d *= mcpRetryBackoffMultiplier
	}
	if d > mcpRetryBackoffCap {
		d = mcpRetryBackoffCap
	}
	return d
}

// mcpJitterFunc returns a pseudo-random duration in [0, max) — the random
// half of mcpRetryBackoff's "equal jitter" (half the base delay fixed,
// half randomized). Jitter matters here for the same reason goal.go's
// goalJitterFunc documents: a cold-start burst hits every affected server
// at once (see the incident in the package doc), so unjittered retries
// would re-hit a still-recovering remote at the exact same instants.
// Real math/rand in production; overridable by tests so the schedule
// stays exactly assertable instead of merely bounded.
var mcpJitterFunc = func(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

// mcpRetryBackoff applies equal jitter to mcpRetryDelay(attempt): half the
// base delay is fixed, the other half is randomized within [0, half) via
// mcpJitterFunc, so the actual wait for attempt N falls in [half, base).
func mcpRetryBackoff(attempt int) time.Duration {
	base := mcpRetryDelay(attempt)
	half := base / 2
	return half + mcpJitterFunc(half)
}

// waitMCPRetryBackoff blocks for mcpRetryBackoff(attempt), or until ctx is
// done, whichever comes first — retryCtx firing (Close) ends the wait
// immediately instead of riding out the rest of the schedule. Uses
// time.NewTimer (not time.After) with an explicit Stop so the timer is
// released promptly when ctx fires first.
func waitMCPRetryBackoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(mcpRetryBackoff(attempt))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryServer is the detached background goroutine for one server whose
// first (or a subsequent) connect attempt failed: it waits the backoff for
// lastAttempt, tries again, and either commits success (populating this
// server's tools into the live merged view and exiting for good — a
// healthy server is never re-probed) or records the new failure and loops
// to wait again, indefinitely, until it succeeds or m.retryCtx is
// cancelled (Close).
//
// Every attempt (and the wait before it) runs under m.retryCtx, never the
// ctx of whichever caller happened to trigger the ORIGINAL first attempt —
// retries are manager-lifetime background work, not request-scoped, the
// same reasoning ensureConnected's connectCtx detachment already
// documents for the first attempt.
func (m *MCPManager) retryServer(name string, lastAttempt int) {
	defer m.retryWG.Done()
	spec := m.servers[name] // m.servers is immutable after construction; safe unguarded read

	attempt := lastAttempt
	for {
		if err := waitMCPRetryBackoff(m.retryCtx, attempt); err != nil {
			return // retryCtx cancelled: Close is tearing the manager down
		}

		client, toolList, err := mcpConnectFunc(m.retryCtx, name, spec)
		attempt++

		m.mu.Lock()
		if m.retryCtx.Err() != nil {
			// Close raced this attempt to completion. Don't commit a
			// connection nobody asked for anymore; if it actually
			// succeeded, close it ourselves so it isn't leaked — Close's
			// own client-closing pass only sees state as of AFTER
			// retryWG.Wait() returns, i.e. after this goroutine is gone,
			// so it can never see (and therefore never close) an entry
			// this branch declines to commit.
			m.mu.Unlock()
			if err == nil && client != nil {
				_ = client.Close()
			}
			return
		}
		entry := m.state[name]
		entry.Attempts = attempt
		if err != nil {
			entry.LastErr = err
			m.mu.Unlock()
			log.Printf("engine: mcp server %q: retry failed: %v (continuing to retry in the background)", name, err)
			if mcpTestRetryCommitted != nil {
				mcpTestRetryCommitted(name, false)
			}
			continue
		}
		entry.Connected = true
		entry.client = client
		entry.tools = mcpBindTools(name, toolList)
		entry.LastErr = nil
		m.rebuildToolsLocked()
		m.mu.Unlock()
		log.Printf("engine: mcp server %q: connected after %d attempt(s)", name, attempt)
		if mcpTestRetryCommitted != nil {
			mcpTestRetryCommitted(name, true)
		}
		return
	}
}

// connectMCPServer builds the right transport for spec, opens a client,
// performs the initialize handshake, and lists all its tools — all bounded
// by spec.ConnectTimeout (or defaultMCPConnectTimeout).
func connectMCPServer(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
	var tr mcp.Transport
	switch {
	case len(spec.Command) > 0:
		tr = &mcp.StdioTransport{Command: spec.Command, Env: spec.Env, Dir: spec.Dir}
	case spec.URL != "":
		tr = &mcp.HTTPTransport{Endpoint: spec.URL, Headers: spec.Headers}
	default:
		return nil, nil, fmt.Errorf("neither command nor url configured")
	}

	client, err := mcp.NewClient(tr, mcp.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}

	timeout := spec.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultMCPConnectTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := client.Initialize(cctx); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}
	tools, err := client.ListAllTools(cctx)
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("tools/list: %w", err)
	}
	return client, tools, nil
}

// Tools implements MCPRegistry.
func (m *MCPManager) Tools(ctx context.Context) []provider.ToolDef {
	m.ensureConnected(ctx)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]provider.ToolDef(nil), m.toolsOrdered...)
}

// CallTool implements MCPRegistry.
func (m *MCPManager) CallTool(ctx context.Context, name string, args json.RawMessage) (message.Parts, bool, error) {
	m.ensureConnected(ctx)
	m.mu.RLock()
	binding, ok := m.tools[name]
	m.mu.RUnlock()
	if !ok {
		return nil, false, fmt.Errorf("engine: mcp: unknown tool %q", name)
	}
	return m.callTool(ctx, binding.server, binding.remote, args)
}

// CallServerTool implements MCPRegistry.
func (m *MCPManager) CallServerTool(ctx context.Context, server, tool string, args json.RawMessage) (message.Parts, bool, error) {
	m.ensureConnected(ctx)
	return m.callTool(ctx, server, tool, args)
}

// callTool distinguishes two different failure shapes the old single
// "server %q is not configured or failed to connect" message used to
// conflate: a server name that was never in the config at all (a caller
// typo, or a plugin's client/mcp.call naming something nonexistent) versus
// one that IS configured but hasn't connected yet, because its first
// attempt failed and it is retrying in the background — the latter is
// recoverable (the very next call after a successful retry succeeds), the
// former never will be.
func (m *MCPManager) callTool(ctx context.Context, server, tool string, args json.RawMessage) (message.Parts, bool, error) {
	// entry's fields are mutated by retryServer under m.mu (see
	// mcpServerEntry's doc comment) — every field this needs (Connected,
	// LastErr, client) must be copied out while still holding the lock,
	// never read from entry afterward, or a concurrent retry commit races
	// this read (caught by -race: see the commit message).
	m.mu.RLock()
	entry, ok := m.state[server]
	var connected bool
	var lastErr error
	var client *mcp.Client
	if ok {
		connected = entry.Connected
		lastErr = entry.LastErr
		client = entry.client
	}
	m.mu.RUnlock()
	if !ok {
		return nil, false, fmt.Errorf("engine: mcp: server %q is not configured", server)
	}
	if !connected {
		return nil, false, fmt.Errorf("engine: mcp: server %q failed to initialize and is retrying in the background: %v", server, lastErr)
	}

	var argVal any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argVal); err != nil {
			return nil, false, fmt.Errorf("engine: mcp: invalid arguments for %s: %w", tool, err)
		}
	}
	res, err := client.CallTool(ctx, tool, argVal)
	if err != nil {
		return nil, false, err
	}
	return mcpContentToParts(server, tool, res.Content), res.IsError, nil
}

// mcpCloseTimeout bounds the whole Close, on top of the bounded timeouts
// each mcp.Client.Close already self-enforces (see that method's doc
// comment) — a defensive outer bound so a pathological client
// implementation can't wedge shutdown indefinitely.
const mcpCloseTimeout = 10 * time.Second

// Close stops every background retry (promptly — synctest bubbles in the
// test suite catch a leak here) and closes every currently-connected
// client concurrently, bounded by mcpCloseTimeout. Safe to call even if no
// server was ever connected.
//
// Close interlocks with ensureConnected via the very same connectOnce
// before it ever reads connection state — but it must never be what
// *initiates* a first connect. connectMCPServer's clients only land in
// m.state at the very end of the one-time first-attempt batch (see
// ensureConnected), so a Close racing a caller's still-in-flight first
// Tools()/CallTool() could otherwise observe no state at all, close
// nothing, and return — moments later the racing connect finishes and
// populates state with a client (or, for a stdio server, a spawned child
// process) nobody will ever close again: a silent leak (this was the
// original Close-vs-first-connect race).
//
// A first fix called ensureConnected(ctx) here unconditionally, which
// correctly interlocks with an in-flight connect but has a second, subtler
// cost: if no caller had ever triggered a connect before Close ran, Close
// itself became that first caller, live-dialing every configured server
// just to immediately close it — up to ConnectTimeout (default 15s) of
// pure shutdown latency for a harness serve process that never actually
// received a prompt, bounded by the wrong timeout entirely (each server's
// own ConnectTimeout, not mcpCloseTimeout, since Close was the one paying
// for the connect rather than merely waiting on one already underway).
//
// Close therefore always calls connectOnce.Do with a no-op, never with the
// real connect closure: sync.Once serializes every Do call regardless of
// which function is passed — only the function belonging to whichever
// caller's Do call is first to run actually executes, and every other
// concurrent (or later) Do call blocks until that one function returns,
// then itself returns without running its own function.
//
//   - If a real connect from some Tools()/CallTool()/CallServerTool caller
//     is already in flight (or has already finished) elsewhere, this call
//     blocks until it completes (preserving the original leak-fix
//     interlock exactly) and then falls through to see the first batch's
//     state fully populated.
//   - If nothing had ever triggered a connect, this call's own no-op wins
//     the race instead: it runs (near-instantly, touching no network),
//     m.state stays nil, and there is nothing to close. Because
//     connectOnce is now permanently consumed, no later
//     Tools()/CallTool()/CallServerTool call — even one arriving after
//     Close has already returned — can start a connect either: it is
//     exactly as if the manager were configured with no servers at all.
//
// After that interlock, Close cancels retryCtx — stopping every background
// retryServer goroutine's current backoff wait, or its in-flight connect
// attempt's context, immediately — and waits for all of them to actually
// exit (retryWG.Wait()) before reading connection state for the final
// close pass. That ordering matters: a retry racing Close to a successful
// connect either commits before the cancellation is observed (and so gets
// closed here, like any other connected client) or discards its own result
// and closes the client itself (see retryServer) — retryWG.Wait()
// guarantees one of those two outcomes has already happened by the time
// this method looks at m.state, so no successfully-connected client from a
// racing retry is ever left unaccounted for.
func (m *MCPManager) Close(ctx context.Context) error {
	m.connectOnce.Do(func() {})

	m.retryCancel()
	m.retryWG.Wait()

	m.mu.RLock()
	var clients []*mcp.Client
	for _, e := range m.state {
		if e.Connected && e.client != nil {
			clients = append(clients, e.client)
		}
	}
	m.mu.RUnlock()
	if len(clients) == 0 {
		return nil
	}

	done := make(chan error, len(clients))
	for _, c := range clients {
		c := c
		go func() { done <- c.Close() }()
	}

	cctx, cancel := context.WithTimeout(ctx, mcpCloseTimeout)
	defer cancel()
	var firstErr error
	for i := 0; i < len(clients); i++ {
		select {
		case err := <-done:
			if err != nil && firstErr == nil {
				firstErr = err
			}
		case <-cctx.Done():
			return cctx.Err()
		}
	}
	return firstErr
}

// mcpContentToParts converts MCP tool-result content into message.Parts.
// message.ToolResult.Content may hold Text and Blob parts only, so image
// and audio content becomes Blob, resource content becomes Text (its
// embedded text) or Blob (its embedded base64 blob), and resource links
// become a descriptive Text line. An empty result still yields one empty
// Text part so a ToolResult is never left with zero content parts.
//
// server and tool are used only to name the offending call in a warning
// log if a Blob's base64 payload turns out to be malformed (see
// decodeMCPBase64); they identify nothing about the payload itself.
func mcpContentToParts(server, tool string, content []mcp.Content) message.Parts {
	var parts message.Parts
	for _, c := range content {
		switch c.Type {
		case mcp.ContentTypeText:
			parts = append(parts, &message.Text{Text: c.Text})
		case mcp.ContentTypeImage, mcp.ContentTypeAudio:
			parts = append(parts, &message.Blob{MediaType: c.MimeType, Data: decodeMCPBase64(server, tool, c.Data)})
		case mcp.ContentTypeResource:
			if c.Resource == nil {
				continue
			}
			if c.Resource.Text != "" {
				parts = append(parts, &message.Text{Text: c.Resource.Text})
			} else if c.Resource.Blob != "" {
				parts = append(parts, &message.Blob{MediaType: c.Resource.MimeType, Data: decodeMCPBase64(server, tool, c.Resource.Blob)})
			}
		case mcp.ContentTypeResourceLink:
			parts = append(parts, &message.Text{Text: fmt.Sprintf("resource: %s (%s)", c.URI, c.Name)})
		default:
			if c.Text != "" {
				parts = append(parts, &message.Text{Text: c.Text})
			}
		}
	}
	if len(parts) == 0 {
		parts = message.Parts{&message.Text{Text: ""}}
	}
	return parts
}

// decodeMCPBase64 decodes s, an MCP content block's base64 payload. On
// malformed base64 it logs a slog warning naming the server and tool the
// payload came from — never the payload bytes themselves, which may be
// arbitrarily large and are not diagnostic here — and returns nil, the
// same fail-open-with-empty-data behavior as before, just no longer
// silent.
func decodeMCPBase64(server, tool, s string) []byte {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		slog.Warn("engine: mcp: malformed base64 content, dropping payload", "server", server, "tool", tool, "error", err)
		return nil
	}
	return data
}
