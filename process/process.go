// Package process manages named, long-lived child processes ("dev
// servers") that an agent starts and later inspects: a `pnpm dev` kept
// alive across tool calls, its combined stdout+stderr streamed to a log
// file, with an optional "ready" gate a caller can block on.
//
// *Manager is a box-scoped singleton, built once per harness process and
// shared across every session it hosts — exactly like engine.MCPManager
// (see that type's doc comment). It is keyed by declared process name;
// every session sharing a Manager sees the same live process state.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// State is a managed process's lifecycle state.
type State string

const (
	// StateStarting is set while Start is blocked waiting for a
	// configured ReadyRegex to match a log line.
	StateStarting State = "starting"
	// StateReady is set once a ReadyRegex match is observed, or
	// immediately on spawn when no ReadyRegex is configured.
	StateReady State = "ready"
	// StateRunning is set when a ready-gated Start's wait times out: the
	// process is left running (never killed by a timeout), but the
	// engine gives up watching for the ready line as part of that Start
	// call. A ReadyRegex match observed later still flips this to
	// StateReady (see Def.ReadyRegex).
	StateRunning State = "running"
	// StateExited is set when the process exits on its own (any exit
	// code), detected asynchronously by the waiter goroutine.
	StateExited State = "exited"
	// StateStopped is set when Stop intentionally terminated the
	// process.
	StateStopped State = "stopped"
)

// isActive reports whether a process in this state currently occupies a
// live slot: Start is idempotent against it, and Declare/Undeclare reject
// mutating a definition while its process is active.
func isActive(s State) bool {
	return s == StateStarting || s == StateReady || s == StateRunning
}

// Origin distinguishes a definition loaded from config at startup from one
// registered at runtime via the process tool's declare action (see
// Manager.Declare). Runtime declarations are server-lifetime only — never
// persisted.
type Origin string

const (
	OriginConfig  Origin = "config"
	OriginRuntime Origin = "runtime"
)

// defaultReadyTimeout is Def.ReadyTimeout's fallback when unset (<= 0),
// mirroring engine.MCPManager's ConnectTimeout default pattern.
const defaultReadyTimeout = 60 * time.Second

// processWaitDelay bounds how long cmd.Wait may block on the command's
// output pipes once the process itself has exited — the same hazard (and
// the same fix) as engine/bash.go's bashWaitDelay: cmd.Stdout/Stderr here
// are not *os.File (they fan out to the log file and the ready-regex
// watcher), which forces os/exec to copy through a goroutine that only
// unblocks on pipe EOF. A backgrounded grandchild holding the write end
// open would otherwise wedge the waiter goroutine forever. Var so tests
// can shrink it.
var processWaitDelay = 2 * time.Second

// Def defines one managed process.
type Def struct {
	// Command is the argv of the process; Command[0] is resolved via PATH
	// like any exec.
	Command []string
	// Dir is the process's working directory. Relative paths are resolved
	// against the Manager's workDir at spawn time.
	Dir string
	// Env is appended to the harness environment when the process is
	// spawned.
	Env []string
	// ReadyRegex, when non-empty, must be a valid RE2 pattern: a
	// combined stdout+stderr log line matching it flips Start's block
	// from starting to ready.
	ReadyRegex string
	// ReadyTimeout bounds Start's blocking wait for ReadyRegex; <= 0
	// defaults to defaultReadyTimeout.
	ReadyTimeout time.Duration
	// Origin records whether this definition came from config or a
	// runtime declare call. Set by the Manager, not the caller.
	Origin Origin
}

// ValidateDef fails loudly on a definition that cannot possibly be spawned:
// a non-empty Command, and — if set — a ReadyRegex that compiles. Used by
// Manager.Declare so a runtime declaration is validated identically to a
// config-file process entry (see config.ProcessSpec's validation, which
// this mirrors error-text-for-error-text).
func ValidateDef(command []string, readyRegex string) error {
	if len(command) == 0 {
		return errors.New("command is required (non-empty argv)")
	}
	if readyRegex != "" {
		if _, err := regexp.Compile(readyRegex); err != nil {
			return fmt.Errorf("invalid ready_regex: %w", err)
		}
	}
	return nil
}

// Status is a point-in-time snapshot of one managed process.
type Status struct {
	Name string `json:"name"`
	// State is one of StateStarting/StateReady/StateRunning/StateExited/
	// StateStopped, or empty for a declared-but-never-started process.
	State      State     `json:"state,omitempty"`
	PID        int       `json:"pid,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// ExitCode/HasExitCode are set once the process has been reaped
	// (State is StateExited or StateStopped).
	ExitCode    int    `json:"exit_code,omitzero"`
	HasExitCode bool   `json:"-"`
	Ready       bool   `json:"ready"`
	Log         string `json:"log"`
	// Note carries a human-readable annotation, e.g. a ready-gate timeout
	// message.
	Note string `json:"note,omitempty"`
}

// Info is a declared process's full definition (env VALUES never
// included — only names, so a status/list response never leaks secrets)
// plus its live Status and Origin.
type Info struct {
	Name         string   `json:"name"`
	Origin       Origin   `json:"origin"`
	Command      []string `json:"command"`
	Dir          string   `json:"dir,omitempty"`
	EnvNames     []string `json:"env_names,omitempty"`
	ReadyRegex   string   `json:"ready_regex,omitempty"`
	ReadyTimeout string   `json:"ready_timeout"`
	Status       Status   `json:"status"`
}

// Manager owns every managed process's definition and live state. Safe for
// concurrent use; built once per harness process (see the package doc) and
// shared across every session.
type Manager struct {
	workDir string

	mu    sync.Mutex
	defs  map[string]Def
	procs map[string]*managedProcess

	everStarted atomicBool
}

// NewManager builds a Manager for the given definitions, resolving
// relative Dir/log paths against workDir. Nothing here spawns anything —
// processes start lazily, on the first Start call for their name.
func NewManager(workDir string, defs map[string]Def) *Manager {
	m := &Manager{
		workDir: workDir,
		defs:    make(map[string]Def, len(defs)),
		procs:   make(map[string]*managedProcess),
	}
	for name, def := range defs {
		if def.Origin == "" {
			def.Origin = OriginConfig
		}
		if def.ReadyTimeout <= 0 {
			def.ReadyTimeout = defaultReadyTimeout
		}
		m.defs[name] = def
	}
	return m
}

// logPath returns the log file path for a managed process name:
// <workDir>/.harness/proc/<name>.log.
func (m *Manager) logPath(name string) string {
	return filepath.Join(m.workDir, ".harness", "proc", name+".log")
}

// EverStarted reports whether any declared process has ever been
// successfully spawned by this Manager, for the lifetime of this process —
// the ambient-status-injection trigger (see engine's request assembly):
// absent until this first flips true, present (reflecting live state) from
// then on, even across processes that have since exited or been stopped.
func (m *Manager) EverStarted() bool {
	return m.everStarted.Load()
}

// Start is idempotent: if name's process is already active (starting,
// ready, or running), its current status is returned unchanged — no
// second process is spawned. Otherwise a fresh process is spawned, its
// combined stdout+stderr streamed to <workDir>/.harness/proc/<name>.log
// (append mode, parent dirs created), and:
//   - no Def.ReadyRegex: the returned status is StateReady immediately.
//   - a Def.ReadyRegex: Start BLOCKS until a log line matches it (status
//     StateReady) or until Def.ReadyTimeout elapses (status StateRunning,
//     Note explains the timeout; the process is left running — a timeout
//     never kills it, and a later match still flips it to StateReady).
//
// ctx bounds the blocking ready-gate wait in addition to ReadyTimeout —
// whichever fires first ends the wait, though only ctx cancellation
// aborts Start without spawning being scoped to it (the spawned process
// itself is never tied to ctx's lifetime; only Stop kills it).
func (m *Manager) Start(ctx context.Context, name string) (Status, error) {
	m.mu.Lock()
	def, ok := m.defs[name]
	if !ok {
		m.mu.Unlock()
		return Status{}, fmt.Errorf("process: unknown process %q", name)
	}
	if p, ok := m.procs[name]; ok {
		st := p.snapshot()
		if isActive(st.State) {
			m.mu.Unlock()
			return st, nil
		}
	}
	m.mu.Unlock()

	p, err := m.spawn(name, def)
	if err != nil {
		return Status{}, err
	}

	m.mu.Lock()
	m.procs[name] = p
	m.mu.Unlock()
	m.everStarted.Store(true)

	return p.awaitReady(ctx, def.ReadyTimeout)
}

// Stop terminates name's process (unix: SIGKILL the whole process group,
// mirroring engine/bash_unix.go's Setpgid/kill-pgroup pattern, so a
// backgrounded grandchild dies with it; non-unix: a plain Kill), waits for
// it to be reaped, and records the exit. A name with no active process is
// a no-op that returns its last known (or zero) status, not an error —
// Stop is meant to be safe to call speculatively (e.g. from Restart).
func (m *Manager) Stop(ctx context.Context, name string) (Status, error) {
	m.mu.Lock()
	if _, ok := m.defs[name]; !ok {
		m.mu.Unlock()
		return Status{}, fmt.Errorf("process: unknown process %q", name)
	}
	p, ok := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return Status{Name: name, Log: m.logPath(name)}, nil
	}
	return p.stop(ctx)
}

// Restart stops name's process (if active) and starts it again fresh.
func (m *Manager) Restart(ctx context.Context, name string) (Status, error) {
	if _, err := m.Stop(ctx, name); err != nil {
		return Status{}, err
	}
	return m.Start(ctx, name)
}

// Status returns name's current status. A declared process that has never
// been started returns a zero-state Status (State "") with its log path
// already populated — the path a future Start will write to.
func (m *Manager) Status(name string) (Status, error) {
	m.mu.Lock()
	_, ok := m.defs[name]
	p := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return Status{}, fmt.Errorf("process: unknown process %q", name)
	}
	if p == nil {
		return Status{Name: name, Log: m.logPath(name)}, nil
	}
	return p.snapshot(), nil
}

// Logs returns the last tail lines of name's log file (empty if the file
// does not exist yet — a declared-but-never-started process) plus its
// current status.
func (m *Manager) Logs(name string, tail int) (string, Status, error) {
	st, err := m.Status(name)
	if err != nil {
		return "", Status{}, err
	}
	if tail <= 0 {
		tail = 50
	}
	content, err := tailFile(st.Log, tail)
	if err != nil {
		if os.IsNotExist(err) {
			return "", st, nil
		}
		return "", st, err
	}
	return content, st, nil
}

// List returns every declared process's full definition and live status,
// sorted by name.
func (m *Manager) List() []Info {
	m.mu.Lock()
	names := make([]string, 0, len(m.defs))
	defs := make(map[string]Def, len(m.defs))
	for name, def := range m.defs {
		names = append(names, name)
		defs[name] = def
	}
	m.mu.Unlock()
	sortStrings(names)

	out := make([]Info, 0, len(names))
	for _, name := range names {
		def := defs[name]
		st, _ := m.Status(name)
		out = append(out, Info{
			Name:         name,
			Origin:       def.Origin,
			Command:      append([]string(nil), def.Command...),
			Dir:          def.Dir,
			EnvNames:     envNames(def.Env),
			ReadyRegex:   def.ReadyRegex,
			ReadyTimeout: def.ReadyTimeout.String(),
			Status:       st,
		})
	}
	return out
}

// Declare registers a new runtime-origin definition, validated identically
// to config parsing (see ValidateDef). Redeclaring a config-origin name is
// rejected; redeclaring a runtime-origin name that is not currently active
// replaces it; redeclaring one that IS active is rejected (stop it
// first). Runtime declarations are server-lifetime only — never persisted
// to any config file.
func (m *Manager) Declare(name string, def Def) error {
	if name == "" {
		return errors.New("process: name is required")
	}
	if err := ValidateDef(def.Command, def.ReadyRegex); err != nil {
		return err
	}
	if def.ReadyTimeout <= 0 {
		def.ReadyTimeout = defaultReadyTimeout
	}
	def.Origin = OriginRuntime

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.defs[name]; ok {
		if existing.Origin == OriginConfig {
			return fmt.Errorf("process: %q is a config-declared process and cannot be redeclared at runtime", name)
		}
		if p, ok := m.procs[name]; ok {
			if st := p.snapshot(); isActive(st.State) {
				return fmt.Errorf("process: %q is currently running; stop it before redeclaring", name)
			}
		}
	}
	m.defs[name] = def
	delete(m.procs, name)
	return nil
}

// Undeclare removes a runtime-origin definition. A config-origin name is
// always rejected. A running runtime-origin process must be stopped
// first.
func (m *Manager) Undeclare(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	def, ok := m.defs[name]
	if !ok {
		return fmt.Errorf("process: unknown process %q", name)
	}
	if def.Origin == OriginConfig {
		return fmt.Errorf("process: %q is config-declared and cannot be undeclared", name)
	}
	if p, ok := m.procs[name]; ok {
		if st := p.snapshot(); isActive(st.State) {
			return fmt.Errorf("process: %q is running; stop it before undeclaring", name)
		}
	}
	delete(m.defs, name)
	delete(m.procs, name)
	return nil
}

// Close stops every currently-active process, bounded by ctx. Intended for
// process shutdown, mirroring engine.MCPManager.Close.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	names := make([]string, 0, len(m.procs))
	for name, p := range m.procs {
		if isActive(p.snapshot().State) {
			names = append(names, name)
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, name := range names {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.Stop(ctx, name)
		}()
	}
	wg.Wait()
}

// envNames extracts the "K" half of every "K=V" entry, dropping values —
// Info must never carry env values (they may be secrets).
func envNames(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for _, kv := range env {
		if i := indexByte(kv, '='); i >= 0 {
			names = append(names, kv[:i])
		} else {
			names = append(names, kv)
		}
	}
	return names
}

func indexByte(s string, b byte) int {
	return bytes.IndexByte([]byte(s), b)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// resolveDir resolves dir against workDir when dir is relative and
// non-empty; an empty dir resolves to workDir itself (the process's
// natural default working directory).
func resolveDir(workDir, dir string) string {
	if dir == "" {
		return workDir
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(workDir, dir)
}

// spawn starts a fresh OS process for def and returns its managedProcess
// handle, already running, before any ready-gate wait.
func (m *Manager) spawn(name string, def Def) (*managedProcess, error) {
	logPath := m.logPath(name)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("process: %s: creating log dir: %w", name, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("process: %s: opening log file: %w", name, err)
	}

	cmd := exec.Command(def.Command[0], def.Command[1:]...)
	cmd.Dir = resolveDir(m.workDir, def.Dir)
	cmd.Env = append(os.Environ(), def.Env...)
	cmd.WaitDelay = processWaitDelay
	configureProcessGroup(cmd)

	p := &managedProcess{
		name:       name,
		def:        def,
		logFile:    logFile,
		logPath:    logPath,
		readyCh:    make(chan struct{}),
		doneCh:     make(chan struct{}),
		state:      StateStarting,
		readyRegex: compileReady(def.ReadyRegex),
	}
	if p.readyRegex == nil {
		// No ready gate configured: ready immediately on spawn.
		p.ready = true
		p.state = StateReady
		close(p.readyCh)
	}

	watcher := &readyWatcher{re: p.readyRegex, onMatch: p.markReady}
	cmd.Stdout = writerFunc(func(b []byte) (int, error) {
		logFile.Write(b) //nolint:errcheck // best-effort log write
		return watcher.Write(b)
	})
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("process: %s: start: %w", name, err)
	}
	p.cmd = cmd
	p.pid = cmd.Process.Pid
	p.startedAt = time.Now().UTC()

	go p.wait()

	return p, nil
}

func compileReady(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		// Unreachable in practice: Manager.Declare/config validation
		// already reject an uncompilable pattern before it ever reaches
		// here. Treat as "no gate" rather than panicking a spawn.
		return nil
	}
	return re
}

// writerFunc adapts a func to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
