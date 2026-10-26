package mcpsrc

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/protocol"
)

// refreshTimeout bounds a read of a tool list, and the wait for one after a missed tool name.
const refreshTimeout = 15 * time.Second

var errRemoved = errors.New("mcp: server removed")

// msg is a command or a report that the actor of a Source handles, one at a time. A worker goroutine does each
// slow step (a dial, a read of a tool list, the close of a client, a retry wait) and reports back with a msg
// that names the entry, so the actor drops the report of an entry that is gone or replaced.
type msg interface{ handle(a *actor) }

// entry is one server of the live set. Only the actor reads or changes it, except name, spec, and ctx, which never change.
type entry struct {
	name   string
	spec   config.MCPServerSpec
	ctx    context.Context
	cancel context.CancelFunc
	// sv is the connected server, with the tool list of now. It is nil until a dial ends well.
	sv *server

	attempts, streak int
	reason           string
	parked           bool

	dialing, wantDial, autoDial, retrying bool
	// attempted is true once the first attempt ended. early holds a client that sent list_changed before its dial ended.
	attempted bool
	early     *mcp.Client
	// readBusy is true while a read of the tool list is in flight, and readAgain when another one is due after it.
	readBusy, readAgain bool
	waiters             []waiter
	refreshWaiters      []chan bool
}

type waiter struct {
	reply  chan connectResult
	dialed bool
}

type connectResult struct {
	dialed bool
	err    error
}

// actor owns the entries of one Source.
type actor struct {
	s       *Source
	entries map[string]*entry
	mem     map[string]*memory
	// closing counts, for each name, the dials and clients of earlier entries that did not report closed. A name dials again only at zero.
	closing map[string]int
	first   map[*entry]bool
	reached []string
	started bool
	frozen  bool
	closed  bool
	after   []func()
	// signalReady asks the loop to close s.ready after it publishes the snapshot, so a reader that wakes reads the frozen state.
	signalReady bool
}

func newActor(s *Source) *actor {
	return &actor{s: s, entries: map[string]*entry{}, mem: map[string]*memory{}, closing: map[string]int{}, first: map[*entry]bool{}}
}

func (a *actor) run() {
	defer close(a.s.done)
	for {
		select {
		case m := <-a.s.inbox:
			m.handle(a)
			a.checkFreeze()
			a.publish()
			for _, f := range a.after {
				f()
			}
			a.after = nil
			if a.signalReady {
				a.signalReady = false
				close(a.s.ready)
			}
		case <-a.s.quit:
			return
		}
	}
}

// send gives m to the actor. It reports false when the actor has ended.
func (s *Source) send(m msg) bool {
	select {
	case s.inbox <- m:
		return true
	case <-s.done:
		return false
	}
}

// spawn runs f as a worker unless the Source is closed.
func (a *actor) spawn(f func()) {
	if a.closed {
		return
	}
	a.s.workers.Go(f)
}

func sameSpec(x, y config.MCPServerSpec) bool {
	return slices.Equal(x.Command, y.Command) && slices.Equal(x.Env, y.Env) && x.Dir == y.Dir && x.URL == y.URL &&
		maps.Equal(x.Headers, y.Headers) && x.ConnectTimeoutS == y.ConnectTimeoutS && x.ToolLoading == y.ToolLoading &&
		x.IncludeSessionIDHeader == y.IncludeSessionIDHeader &&
		x.ToolTimeout() == y.ToolTimeout()
}

func cloneSpec(sp config.MCPServerSpec) config.MCPServerSpec {
	sp.Command, sp.Env, sp.Headers = slices.Clone(sp.Command), slices.Clone(sp.Env), maps.Clone(sp.Headers)
	if sp.ToolTimeoutS != nil {
		t := *sp.ToolTimeoutS
		sp.ToolTimeoutS = &t
	}
	return sp
}

// add makes the entry of a server and the memory of its name.
func (a *actor) add(name string, spec config.MCPServerSpec) *entry {
	e := &entry{name: name, spec: cloneSpec(spec)}
	e.ctx, e.cancel = context.WithCancel(a.s.ctx)
	a.entries[name] = e
	loading := cmp.Or(spec.ToolLoading, a.s.mode, "auto")
	switch m := a.mem[name]; {
	case m == nil:
		a.mem[name] = &memory{loading: loading, seen: map[string]mcp.Tool{}, base: map[string]bool{}, sessionIDHeader: spec.IncludeSessionIDHeader && spec.URL != ""}
	default:
		c := *m
		c.sessionIDHeader = spec.IncludeSessionIDHeader && spec.URL != ""
		if len(m.base) == 0 {
			c.loading = loading
		}
		a.mem[name] = &c
	}
	return e
}

// remove drops e. Its client closes on a worker, and the next entry of the name waits for that.
func (a *actor) remove(e *entry) {
	delete(a.entries, e.name)
	delete(a.first, e)
	e.cancel()
	switch {
	case e.sv.up():
		a.closing[e.name]++
		a.closeClient(e.name, e.sv.client)
	case e.dialing:
		a.closing[e.name]++
	}
	e.sv = nil
	for _, w := range e.waiters {
		w.reply <- connectResult{err: errRemoved}
	}
	e.waiters = nil
	for _, w := range e.refreshWaiters {
		w <- false
	}
	e.refreshWaiters = nil
}

func (a *actor) closeClient(name string, c *mcp.Client) {
	if a.closed {
		_ = c.Close()
		return
	}
	a.spawn(func() {
		_ = c.Close()
		a.s.post(closedMsg{name})
	})
}

// post gives m to the actor from a worker. It drops m when the actor has ended.
func (s *Source) post(m msg) { s.send(m) }

// postUntil is post for a callback of a transport: it gives up when ctx ends, so a client that waits to close never
// waits for a transport goroutine that waits for the actor.
func (s *Source) postUntil(ctx context.Context, m msg) {
	select {
	case s.inbox <- m:
	case <-s.done:
	case <-ctx.Done():
	}
}

// closedOne notes that one earlier dial or client of name ended, and starts the dial that waited for it.
func (a *actor) closedOne(name string) {
	if a.closing[name]--; a.closing[name] > 0 {
		return
	}
	delete(a.closing, name)
	if e := a.entries[name]; e != nil && e.wantDial {
		a.startDial(e, e.autoDial)
	}
}

// startDial dials e on a worker, unless it is connected or dialing. An earlier client of the name that has not
// closed holds it back.
func (a *actor) startDial(e *entry, auto bool) {
	if e.dialing || e.sv.up() || a.closed {
		return
	}
	if a.closing[e.name] > 0 {
		e.wantDial, e.autoDial = true, e.autoDial || auto
		return
	}
	e.dialing, e.wantDial, e.autoDial = true, false, auto
	s := a.s
	a.spawn(func() {
		sv, err := dial(e.ctx, e.spec, s.workDir, s.httpClient, func(c *mcp.Client) { s.postUntil(e.ctx, changedMsg{e, c}) })
		s.post(dialedMsg{e: e, sv: sv, err: err})
	})
}

// startFirst begins the first connect of the entries that exist now.
func (a *actor) startFirst() {
	a.started = true
	for _, name := range slices.Sorted(maps.Keys(a.entries)) {
		e := a.entries[name]
		a.first[e] = true
		a.startDial(e, true)
	}
}

// checkFreeze ends the first connect when every entry of it ended its first attempt and has no read of its tool
// list in flight. The tools that each server lists then are the tools of the first connect.
func (a *actor) checkFreeze() {
	if !a.started || a.frozen {
		return
	}
	for e := range a.first {
		if !e.attempted || e.readBusy || e.readAgain {
			return
		}
	}
	a.frozen = true
	for e := range a.first {
		if e.sv.up() {
			a.reached = append(a.reached, e.name)
			a.freeze(e)
		}
	}
	slices.Sort(a.reached)
	a.signalReady = true
}

func (a *actor) freeze(e *entry) {
	m := *a.mem[e.name]
	m.base = map[string]bool{}
	for _, t := range e.sv.tools {
		m.base[t.Name] = true
	}
	m.inst, m.resources, m.servesSkills, m.skills = e.sv.client.Instructions(), e.sv.resources(), e.sv.servesSkills(), e.sv.skills
	a.mem[e.name] = &m
}

// remember adds the tools of sv to what the runtime read of the server.
func (a *actor) remember(name string, sv *server) {
	m := *a.mem[name]
	m.up, m.seen = true, maps.Clone(m.seen)
	for _, t := range sv.tools {
		if _, ok := m.seen[t.Name]; !ok {
			m.seen[t.Name] = t
		}
	}
	a.mem[name] = &m
}

// startRead reads the tool list of e on a worker. One read runs at a time for an entry: a notification that comes
// while a read runs makes one more read after it.
func (a *actor) startRead(e *entry) {
	if e.readBusy {
		e.readAgain = true
		return
	}
	e.readBusy = true
	c := e.sv.client
	s := a.s
	a.spawn(func() {
		ctx, cancel := context.WithTimeout(e.ctx, refreshTimeout)
		defer cancel()
		tools, err := c.ListAllTools(ctx)
		slices.SortFunc(tools, func(x, y mcp.Tool) int { return strings.Compare(x.Name, y.Name) })
		s.post(listedMsg{e: e, client: c, tools: tools, err: err})
	})
}

func (a *actor) retryLater(e *entry) {
	if e.retrying || a.closed {
		return
	}
	e.retrying = true
	wait := retryWait(e.streak - 1)
	if retriesHeld.Load() {
		wait = time.Duration(math.MaxInt64)
	}
	s := a.s
	a.spawn(func() {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
			s.post(retryMsg{e})
		case <-e.ctx.Done():
		}
	})
}

// statuses returns the state of each server of the Source, in name order.
func (a *actor) statuses() []protocol.MCPServerStatus {
	out := make([]protocol.MCPServerStatus, 0, len(a.entries))
	for _, name := range slices.Sorted(maps.Keys(a.entries)) {
		e := a.entries[name]
		out = append(out, protocol.MCPServerStatus{Name: name, Connected: e.sv.up(), Attempts: e.attempts, Parked: e.parked, Reason: e.reason})
	}
	return out
}

// publish makes the immutable snapshot of the state, and wakes the readers that wait for a change.
func (a *actor) publish() {
	sn := &snapshot{live: map[string]*server{}, life: map[string]context.Context{}, health: map[string]health{},
		mem: maps.Clone(a.mem), reached: slices.Clone(a.reached), changed: make(chan struct{})}
	for name, e := range a.entries {
		sn.names = append(sn.names, name)
		sn.health[name] = health{attempts: e.attempts, reason: e.reason, parked: e.parked}
		if e.sv.up() {
			sn.live[name], sn.life[name] = e.sv, e.ctx
		}
		if e.readBusy || e.readAgain {
			sn.reads++
		}
	}
	slices.Sort(sn.names)
	if old := a.s.snap.Swap(sn); old != nil {
		close(old.changed)
	}
}

type startMsg struct{}

func (startMsg) handle(a *actor) {
	if !a.started && !a.closed {
		a.startFirst()
	}
}

// setMsg replaces the servers of the Source.
type setMsg struct {
	servers map[string]config.MCPServerSpec
	reply   chan []protocol.MCPServerStatus
}

func (m setMsg) handle(a *actor) {
	if a.closed {
		m.reply <- nil
		return
	}
	for _, name := range slices.Sorted(maps.Keys(a.entries)) {
		e := a.entries[name]
		if sp, ok := m.servers[name]; !ok || !sameSpec(e.spec, sp) {
			a.remove(e)
		}
	}
	var fresh []*entry
	for _, name := range slices.Sorted(maps.Keys(m.servers)) {
		if a.entries[name] == nil {
			fresh = append(fresh, a.add(name, m.servers[name]))
		}
	}
	switch {
	case a.started:
		for _, e := range fresh {
			a.startDial(e, true)
		}
	case len(a.entries) > 0:
		a.startFirst()
	}
	statuses := a.statuses()
	a.after = append(a.after, func() { m.reply <- statuses })
}

// connectMsg asks for one attempt to connect the server name.
type connectMsg struct {
	name  string
	reply chan connectResult
}

func (m connectMsg) handle(a *actor) {
	e := a.entries[m.name]
	switch {
	case e == nil:
		m.reply <- connectResult{err: errRemoved}
	case e.sv.up():
		m.reply <- connectResult{}
	case e.dialing:
		e.waiters = append(e.waiters, waiter{m.reply, false})
	default:
		e.waiters = append(e.waiters, waiter{m.reply, true})
		a.startDial(e, false)
	}
}

// dialedMsg reports the end of a dial.
type dialedMsg struct {
	e   *entry
	sv  *server
	err error
}

func (m dialedMsg) handle(a *actor) {
	e := m.e
	if a.entries[e.name] != e || a.closed {
		switch {
		case a.closed && m.sv.up():
			_ = m.sv.client.Close()
		case a.closed:
		case m.sv.up():
			a.closeClient(e.name, m.sv.client)
		default:
			a.closedOne(e.name)
		}
		return
	}
	e.dialing, e.attempted = false, true
	e.attempts++
	waiters := e.waiters
	e.waiters = nil
	if m.err != nil {
		e.reason = connectReason(m.err)
		if e.autoDial {
			a.failed(e, m.err)
		}
		for _, w := range waiters {
			w.reply <- connectResult{dialed: w.dialed, err: m.err}
		}
		return
	}
	if e.streak > 0 {
		a.s.log.Info("mcp: server connected after a retry", "server", e.name)
	}
	e.sv, e.reason, e.parked, e.streak = m.sv, "", false, 0
	if m.sv.skillsErr != nil {
		a.s.log.Warn("mcp: skills left out of the list", "server", e.name, "err", hide(e.name, m.sv.skillsErr))
	}
	a.remember(e.name, e.sv)
	for _, w := range waiters {
		w.reply <- connectResult{dialed: w.dialed}
	}
	if e.early == m.sv.client {
		e.early = nil
		a.startRead(e)
	}
}

// failed counts a failed attempt of the automatic dials of e: it retries in the background, and parks the server after
// maxRetries failed retries.
func (a *actor) failed(e *entry, err error) {
	msg := "mcp: retry did not connect"
	if e.streak == 0 {
		msg = "mcp: server did not connect"
	}
	a.s.log.Warn(msg, "server", e.name, "err", hide(e.name, err))
	if e.streak++; e.streak > maxRetries {
		e.parked = true
		a.s.log.Warn("mcp: server parked; the mcp tool can connect it", "server", e.name)
		return
	}
	a.retryLater(e)
}

// retryMsg reports that the wait before a retry of e ended.
type retryMsg struct{ e *entry }

func (m retryMsg) handle(a *actor) {
	m.e.retrying = false
	if a.entries[m.e.name] == m.e && !m.e.sv.up() {
		a.startDial(m.e, true)
	}
}

// changedMsg reports a list_changed notification of the client of e.
type changedMsg struct {
	e      *entry
	client *mcp.Client
}

func (m changedMsg) handle(a *actor) {
	e := m.e
	switch {
	case a.entries[e.name] != e || a.closed:
	case e.sv.up() && e.sv.client == m.client:
		a.startRead(e)
	case e.dialing:
		e.early = m.client
	}
}

// refreshMsg requests a tool-list read of a connected server.
type refreshMsg struct {
	name  string
	reply chan bool
}

func (m refreshMsg) handle(a *actor) {
	e := a.entries[m.name]
	if a.closed || e == nil || !e.sv.up() {
		m.reply <- false
		return
	}
	e.refreshWaiters = append(e.refreshWaiters, m.reply)
	a.startRead(e)
}

// listedMsg reports the end of a read of the tool list of a client.
type listedMsg struct {
	e      *entry
	client *mcp.Client
	tools  []mcp.Tool
	err    error
}

func (m listedMsg) handle(a *actor) {
	e := m.e
	if a.entries[e.name] != e || !e.sv.up() || e.sv.client != m.client || a.closed {
		return
	}
	e.readBusy = false
	if m.err == nil {
		e.sv = newServer(m.client, m.tools, e.spec.ToolTimeout())
		a.remember(e.name, e.sv)
	} else if e.ctx.Err() == nil {
		a.s.log.Warn("mcp: tool list did not refresh", "server", e.name, "err", hide(e.name, m.err))
	}
	if e.readAgain {
		e.readAgain = false
		a.startRead(e)
		return
	}
	waiters := e.refreshWaiters
	e.refreshWaiters = nil
	// A waiter reads the view, so it wakes after the loop publishes the new list.
	a.after = append(a.after, func() {
		for _, reply := range waiters {
			reply <- true
		}
	})
}

// closedMsg reports that a client of name closed.
type closedMsg struct{ name string }

func (m closedMsg) handle(a *actor) {
	if !a.closed {
		a.closedOne(m.name)
	}
}

// closeMsg ends the Source: it cancels every entry and closes every client.
type closeMsg struct{ reply chan struct{} }

func (m closeMsg) handle(a *actor) {
	a.closed = true
	a.s.cancel()
	var wg sync.WaitGroup
	for _, e := range a.entries {
		if c := e.sv.clientOf(); c != nil {
			wg.Go(func() { _ = c.Close() })
		}
		for _, w := range e.waiters {
			w.reply <- connectResult{err: errRemoved}
		}
		e.sv, e.waiters = nil, nil
	}
	wg.Wait()
	clear(a.entries)
	if !a.frozen {
		a.frozen = true
		a.signalReady = true
	}
	close(m.reply)
}
