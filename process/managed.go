package process

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// managedProcess is the live handle for one spawned OS process: its
// *exec.Cmd, the log file it writes to, and the mutable status fields a
// concurrent Status()/Stop()/the waiter goroutine all touch under mu.
type managedProcess struct {
	name string
	def  Def
	cmd  *exec.Cmd

	logFile *os.File
	logPath string

	readyRegex *regexp.Regexp
	// readyCh is closed exactly once, the moment the process becomes
	// ready (immediately at spawn with no ReadyRegex, or on the first
	// matching log line otherwise).
	readyCh   chan struct{}
	readyOnce sync.Once
	// doneCh is closed exactly once, when the waiter goroutine has
	// reaped the process (cmd.Wait returned).
	doneCh chan struct{}

	mu            sync.Mutex
	state         State
	ready         bool
	pid           int
	startedAt     time.Time
	finishedAt    time.Time
	exitCode      int
	hasExitCode   bool
	note          string
	stopRequested bool
}

// markReady is the readyWatcher's onMatch callback: it flips ready/state
// to StateReady (unless the process has already exited/stopped, in which
// case there is nothing left to mark ready) and closes readyCh exactly
// once, waking any Start call still blocked in awaitReady.
func (p *managedProcess) markReady() {
	p.mu.Lock()
	if !isActive(p.state) && p.state != "" {
		p.mu.Unlock()
		return
	}
	p.ready = true
	p.state = StateReady
	p.mu.Unlock()
	p.readyOnce.Do(func() { close(p.readyCh) })
}

// wait runs cmd.Wait() exactly once for this process's lifetime (the
// sanctioned single caller of Wait; Stop must never call it a second
// time), then records the exit and closes doneCh — this is the
// asynchronous death-detection goroutine: a client never has to poll to
// discover a process died on its own.
func (p *managedProcess) wait() {
	err := p.cmd.Wait()
	_ = p.logFile.Close()

	p.mu.Lock()
	p.finishedAt = time.Now().UTC()
	p.hasExitCode = true
	if p.cmd.ProcessState != nil {
		p.exitCode = p.cmd.ProcessState.ExitCode()
	} else if err != nil {
		p.exitCode = -1
	}
	if p.stopRequested {
		p.state = StateStopped
	} else {
		p.state = StateExited
	}
	p.mu.Unlock()

	close(p.doneCh)
}

// snapshot returns a copy of the process's current Status.
func (p *managedProcess) snapshot() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Status{
		Name:        p.name,
		State:       p.state,
		PID:         p.pid,
		StartedAt:   p.startedAt,
		FinishedAt:  p.finishedAt,
		ExitCode:    p.exitCode,
		HasExitCode: p.hasExitCode,
		Ready:       p.ready,
		Log:         p.logPath,
		Note:        p.note,
	}
}

// awaitReady blocks Start's caller until the process becomes ready, dies
// on its own, ctx is canceled, or timeout elapses — whichever comes
// first. A timeout leaves the process running (never kills it) and
// records a Note explaining the timeout; the readyRegex watcher keeps
// running, so a later match still flips state to StateReady even after
// this call has already returned.
func (p *managedProcess) awaitReady(ctx context.Context, timeout time.Duration) (Status, error) {
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-p.readyCh:
	case <-p.doneCh:
		// The process exited before ever becoming ready.
	case <-timer.C:
		p.noteReadyTimeout(timeout)
	case <-ctx.Done():
		return p.snapshot(), ctx.Err()
	}
	return p.snapshot(), nil
}

// noteReadyTimeout records that the ready gate elapsed. Extracted (and
// guarded on exactly StateStarting) because Go's select picks randomly
// among simultaneously-ready cases: a process whose ready line landed at
// the same instant the timer fired must keep its fresh StateReady rather
// than being overwritten with a misleading "timed out" note. The only
// transition this represents is starting -> running.
func (p *managedProcess) noteReadyTimeout(timeout time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == StateStarting {
		p.state = StateRunning
		p.note = "ready gate timed out after " + timeout.String() + "; process left running"
	}
}

// stop kills the process (see killProcess) and blocks until the waiter
// goroutine has reaped it, bounded by processWaitDelay (plus its own
// retry margin) so a pathological process can never wedge Stop forever.
func (p *managedProcess) stop(ctx context.Context) (Status, error) {
	p.mu.Lock()
	alreadyDone := !isActive(p.state)
	p.stopRequested = true
	pid := p.pid
	p.mu.Unlock()

	if alreadyDone {
		return p.snapshot(), nil
	}

	killProcess(p.cmd, pid)

	deadline := time.NewTimer(processWaitDelay*2 + 2*time.Second)
	defer deadline.Stop()
	select {
	case <-p.doneCh:
	case <-ctx.Done():
		return p.snapshot(), ctx.Err()
	case <-deadline.C:
		// Best-effort: return whatever is known rather than hang forever.
	}
	return p.snapshot(), nil
}

// readyWatcher scans bytes written to a process's combined stdout+stderr
// for the first line matching its regex, calling onMatch exactly once
// when found. A nil regex never matches (used when no ReadyRegex is
// configured — the process is marked ready at spawn time instead, see
// Manager.spawn).
type readyWatcher struct {
	re      *regexp.Regexp
	onMatch func()

	mu      sync.Mutex
	buf     []byte
	matched bool
}

// readyWatcherWindow bounds the sliding buffer readyWatcher matches
// against, so a process that never becomes ready cannot grow this
// unbounded over a long run.
const readyWatcherWindow = 8192

func (w *readyWatcher) Write(p []byte) (int, error) {
	if w.re == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.matched {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	if len(w.buf) > readyWatcherWindow {
		w.buf = w.buf[len(w.buf)-readyWatcherWindow:]
	}
	if w.re.Match(w.buf) {
		w.matched = true
		w.onMatch()
	}
	return len(p), nil
}

// atomicBool is a tiny bool wrapper over atomic.Bool's int32 encoding
// (kept local so this package has no other dependency beyond the
// standard library).
type atomicBool struct {
	v atomic.Bool
}

func (b *atomicBool) Load() bool   { return b.v.Load() }
func (b *atomicBool) Store(v bool) { b.v.Store(v) }
