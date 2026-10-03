// Package external holds what every third-party harness backend shares:
// process supervision, a JSON line transport over stdin and stdout, and the
// mirror of the external transcript that is saved as one state blob.
package external

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const stderrCap = 4096

// Process is one external harness process in its own process group. It
// reads stdout as lines and writes JSON lines to stdin.
type Process struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	lines  chan []byte
	stderr *capped
	exited chan struct{}
	quit   chan struct{}
	err    error
	once   sync.Once
}

// Start starts cmd in a new process group. cmd must have no stdio set.
func Start(cmd *exec.Cmd, waitDelay time.Duration) (*Process, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW)
		return nil, err
	}
	p := &Process{cmd: cmd, stdin: inW, stdout: outR, lines: make(chan []byte), stderr: &capped{},
		exited: make(chan struct{}), quit: make(chan struct{})}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, p.stderr
	cmd.WaitDelay = waitDelay
	ownGroup(cmd)
	err = cmd.Start()
	closeAll(inR, outW)
	if err != nil {
		closeAll(inW, outR)
		return nil, err
	}
	go p.read()
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		_ = f.Close()
	}
}

func (p *Process) read() {
	defer close(p.lines)
	sc := bufio.NewScanner(p.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		select {
		case p.lines <- line:
		case <-p.quit:
			return
		}
	}
}

// Lines yields each stdout line. It closes at the end of stdout.
func (p *Process) Lines() <-chan []byte { return p.lines }

// Send writes v to stdin as one JSON line.
func (p *Process) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// CloseInput closes stdin. The external harness ends after its last turn.
func (p *Process) CloseInput() { _ = p.stdin.Close() }

// Interrupt sends SIGINT to the process group and unblocks a pending Send.
func (p *Process) Interrupt() {
	if !p.done() {
		interruptGroup(p.cmd.Process)
	}
	_ = p.stdin.SetWriteDeadline(time.Now())
}

func (p *Process) done() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// Finish passes each remaining stdout line to each until stdout ends and
// the process exits. After grace, it kills a process that has not exited
// and stops reading. A descendant of an exited process keeps running. It
// returns the exit error with the tail of stderr.
func (p *Process) Finish(grace time.Duration, each func([]byte)) error {
	t := time.NewTimer(grace)
	defer t.Stop()
	expired := false
	for lines := p.lines; lines != nil; {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
			} else if each != nil {
				each(line)
			}
		case <-t.C:
			expired = true
			p.abandon()
		}
	}
	if !expired {
		select {
		case <-p.exited:
		case <-t.C:
			killGroup(p.cmd.Process)
		}
	}
	<-p.exited
	p.abandon()
	if p.err != nil {
		if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
			return fmt.Errorf("%w: %s", p.err, tail)
		}
	}
	return p.err
}

func (p *Process) abandon() {
	if !p.done() {
		killGroup(p.cmd.Process)
	}
	p.once.Do(func() {
		close(p.quit)
		closeAll(p.stdin, p.stdout)
	})
}

type capped struct {
	mu  sync.Mutex
	buf []byte
}

func (c *capped) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := stderrCap - len(c.buf); room > 0 {
		c.buf = append(c.buf, b[:min(room, len(b))]...)
	}
	return len(b), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}
