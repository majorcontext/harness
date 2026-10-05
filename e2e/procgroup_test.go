package e2e

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

const termGrace = 10 * time.Second

// cleanupGrace bounds the SIGTERM wait at test cleanup. A process that is
// still draining at the bound is killed, which loses only its coverage counters.
const cleanupGrace = 3 * time.Second

var liveGroups = struct {
	sync.Mutex
	m map[int]*procGroup
}{m: map[int]*procGroup{}}

// procGroup is a subprocess that leads its own process group. kill removes
// the whole group, so a child of the process dies with it.
type procGroup struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

// startGroup starts cmd in a new process group and registers its shutdown
// with t.Cleanup before it returns. Shutdown sends SIGTERM first, so an
// instrumented binary flushes its coverage counters, then SIGKILL.
func startGroup(t *testing.T, cmd *exec.Cmd) *procGroup {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", cmd.Path, err)
	}
	g := &procGroup{cmd: cmd, exited: make(chan struct{})}
	pid := cmd.Process.Pid
	liveGroups.Lock()
	liveGroups.m[pid] = g
	liveGroups.Unlock()
	t.Cleanup(func() { _ = g.stopGracefully(cleanupGrace) })
	go func() {
		_ = cmd.Wait()
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		liveGroups.Lock()
		delete(liveGroups.m, pid)
		liveGroups.Unlock()
		close(g.exited)
	}()
	return g
}

func (g *procGroup) alive() bool {
	select {
	case <-g.exited:
		return false
	default:
		return true
	}
}

// kill sends SIGKILL to the process group and waits for the leader to exit.
func (g *procGroup) kill() {
	if g.alive() {
		_ = syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL)
	}
	<-g.exited
}

// terminate sends SIGTERM and waits for a clean exit. A process that ignores
// SIGTERM for termGrace is killed and the test fails.
func (g *procGroup) terminate(t *testing.T) {
	t.Helper()
	if err := g.stopGracefully(termGrace); err != nil {
		t.Error(err)
	}
}

func (g *procGroup) stopGracefully(grace time.Duration) error {
	if !g.alive() {
		return nil
	}
	if err := g.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("SIGTERM: %w", err)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-g.exited:
		return nil
	case <-timer.C:
		g.kill()
		return fmt.Errorf("process %d ignored SIGTERM for %s; killed it", g.cmd.Process.Pid, grace)
	}
}

// killLiveGroups kills every group still registered and returns their pids.
func killLiveGroups() []int {
	liveGroups.Lock()
	var leaked []*procGroup
	for _, g := range liveGroups.m {
		leaked = append(leaked, g)
	}
	liveGroups.Unlock()
	var pids []int
	for _, g := range leaked {
		pids = append(pids, g.cmd.Process.Pid)
		g.kill()
	}
	return pids
}

// guardProcessGroups kills every live group when the test binary receives
// SIGINT or SIGTERM, and shortly before the go test timeout. Those two paths
// skip t.Cleanup, and a group leader does not share the terminal's
// foreground group, so it would otherwise outlive the binary.
func guardProcessGroups() (stop func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig, ok := <-sigs
		if !ok {
			return
		}
		killLiveGroups()
		signal.Reset(sig)
		_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
	}()
	var timer *time.Timer
	if f, ok := flag.Lookup("test.timeout").Value.(flag.Getter); ok {
		if d, ok := f.Get().(time.Duration); ok && d > 0 {
			timer = time.AfterFunc(d-min(d/10, 30*time.Second), func() { killLiveGroups() })
		}
	}
	return func() {
		signal.Stop(sigs)
		close(sigs)
		if timer != nil {
			timer.Stop()
		}
	}
}

func reportLeakedGroups() bool {
	pids := killLiveGroups()
	if len(pids) > 0 {
		fmt.Fprintf(os.Stderr, "e2e: subprocess groups outlived their tests: %v\n", pids)
	}
	return len(pids) > 0
}

func gone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

func requireGone(t *testing.T, what string, pid int) {
	t.Helper()
	testpoll.Until(t, 10*time.Second, fmt.Sprintf("%s %d outlived its test", what, pid), func() bool { return gone(pid) })
	if !testpoll.UntilNoT(10*time.Second, func() bool { return gone(-pid) }) {
		t.Errorf("process group %d outlived its test; pids still present: %v", pid, groupMembers(pid))
	}
}

func groupMembers(pgid int) []int {
	out, err := exec.Command("ps", "-axo", "pid=,pgid=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		var p, g int
		if n, _ := fmt.Sscan(line, &p, &g); n == 2 && g == pgid {
			pids = append(pids, p)
		}
	}
	return pids
}

func shellGroup(t *testing.T, script string) (g *procGroup, grandchild int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	g = startGroup(t, cmd)
	if _, err := fmt.Fscan(bufio.NewReader(out), &grandchild); err != nil {
		t.Fatalf("reading grandchild pid: %v", err)
	}
	return g, grandchild
}

func TestKillRemovesDescendants(t *testing.T) {
	skipShort(t)
	var leader, child int
	t.Run("body", func(t *testing.T) {
		var g *procGroup
		g, child = shellGroup(t, "sleep 600 & echo $!; wait")
		leader = g.cmd.Process.Pid
	})
	requireGone(t, "leader", leader)
	requireGone(t, "descendant", child)
}

func TestTerminateKillsAProcessThatIgnoresSIGTERM(t *testing.T) {
	skipShort(t)
	g, child := shellGroup(t, "trap '' TERM; sleep 600 & echo $!; wait")
	if err := g.stopGracefully(100 * time.Millisecond); err == nil {
		t.Fatal("stopGracefully = nil for a process that ignores SIGTERM, want an error")
	}
	requireGone(t, "leader", g.cmd.Process.Pid)
	requireGone(t, "descendant", child)
}

func TestServeProcessesDieWithTheirTest(t *testing.T) {
	skipShort(t)
	var pids []int
	t.Run("body", func(t *testing.T) {
		fake := harnesstest.New(t)
		d := newHTTPDriver(t, fake.URL())
		pids = append(pids, d.p.cmd.Process.Pid)
		d.Restart(t, true)
		pids = append(pids, d.p.cmd.Process.Pid)
		d.Restart(t, false)
		pids = append(pids, d.p.cmd.Process.Pid)
	})
	if len(pids) != 3 {
		t.Fatalf("recorded %d serve pids, want 3", len(pids))
	}
	for _, pid := range pids {
		requireGone(t, "serve", pid)
	}
}

func TestCleanupSendsSIGTERMBeforeSIGKILL(t *testing.T) {
	skipShort(t)
	marker := filepath.Join(t.TempDir(), "got-term")
	t.Run("body", func(t *testing.T) {
		shellGroup(t, "trap 'touch "+marker+"; exit 0' TERM; sleep 600 & echo $!; while :; do sleep 0.01; done")
	})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("process did not see SIGTERM at test cleanup: %v", err)
	}
}
