//go:build unix

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// ptySupported gates handleTerm's platform check: true on every unix GOOS
// this file builds under (see the !unix stub in term_other.go).
const ptySupported = true

// termKillWindow mirrors bashGroupKillWindow's retry loop in
// engine/bash_unix.go (see its doc comment for why one SIGKILL isn't always
// enough — a straggler forked just before the signal lands). Tests override
// this var to keep wall-clock cost small.
var termKillWindow = 200 * time.Millisecond

// killProcessGroup SIGKILLs every process in pgid, retrying for a short,
// bounded window until the group is confirmed empty. This is the exact
// pattern engine/bash_unix.go's killProcessGroup uses for the bash tool;
// duplicated here (rather than exported from engine, which the server
// package does not otherwise depend on for process control) because a PTY
// session's shell is its own process-group leader for the same reason a
// bash invocation's sh is: a backgrounded grandchild must die with it, not
// survive as an orphan holding the pty's fd open.
func killProcessGroup(pgid int) {
	deadline := time.Now().Add(termKillWindow)
	for {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && errors.Is(err, syscall.ESRCH) {
			return // no process in the group remains
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// termShell resolves the shell /term spawns: the user's own $SHELL if set,
// falling back to sh (the same fallback the bash tool uses for -c, though
// here it is run interactively rather than with -c).
func termShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "sh"
}

// termControlMsg is the shape of a client TEXT frame: a JSON control
// message. "resize" is the only type defined today; an unknown type or a
// message that fails to decode is silently ignored rather than tearing
// down the session over a forward-compatibility hiccup.
type termControlMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// defaultTermCols/Rows seed the PTY before the client's first resize
// control message arrives (xterm's fit addon sends one immediately on
// open, so this is a brief default, not the steady state).
const (
	defaultTermCols = 80
	defaultTermRows = 24
)

// runTerminal spawns termShell() in a PTY rooted at workDir and relays
// bytes bidirectionally with conn until either side ends:
//
//   - PTY output -> binary WebSocket frames.
//   - Client binary frames -> PTY input.
//   - Client TEXT frames -> control messages (see termControlMsg), applied
//     to the PTY winsize.
//   - The shell exiting on its own closes conn with a normal closure frame.
//   - The client disconnecting (a WebSocket read error) kills the whole
//     shell process group (see killProcessGroup) rather than leaving it
//     running unattended or orphaning a backgrounded grandchild.
//   - ctx (the originating HTTP request's context) ending — e.g. server
//     shutdown — does the same.
func runTerminal(ctx context.Context, conn *websocket.Conn, workDir string) {
	cmd := exec.Command(termShell())
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	// pty.StartWithSize puts the shell in its own session (Setsid) with
	// this pty as its controlling terminal (Setctty) — which also makes it
	// its own process-group leader, so killProcessGroup(cmd.Process.Pid)
	// below reaches the whole tree, exactly like the bash tool's
	// Setpgid-based group kill.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: defaultTermCols, Rows: defaultTermRows})
	if err != nil {
		conn.Close(websocket.StatusInternalError, "spawn shell: "+err.Error())
		return
	}

	shellDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(shellDone)
	}()

	// PTY output -> binary frames. Ends (closing outDone) when the PTY
	// closes (read error/EOF, e.g. the shell exited) or a write to conn
	// fails (the client is gone).
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := conn.Write(context.Background(), websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Client frames -> PTY input / control messages. Ends (delivering to
	// wsErr) the instant the client disconnects — a closed WebSocket, a
	// dropped TCP connection, or an explicit close frame all surface here
	// as a Read error, which is exactly the "client disconnected" signal
	// the select below acts on.
	wsErr := make(chan error, 1)
	go func() {
		for {
			typ, data, err := conn.Read(context.Background())
			if err != nil {
				wsErr <- err
				return
			}
			switch typ {
			case websocket.MessageBinary:
				_, _ = ptmx.Write(data)
			case websocket.MessageText:
				var m termControlMsg
				if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
					_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(m.Cols), Rows: uint16(m.Rows)})
				}
			}
		}
	}()

	select {
	case <-shellDone:
		// The shell exited on its own (e.g. `exit`, or the command it was
		// running finished and it was configured to quit) — a normal,
		// expected end. Close with a normal closure frame.
		conn.Close(websocket.StatusNormalClosure, "shell exited")
	case <-wsErr:
		// The client disconnected. Kill the shell's whole process group
		// rather than leaving an unattended shell (and anything it may
		// have backgrounded) running forever.
		if cmd.Process != nil {
			killProcessGroup(cmd.Process.Pid)
		}
		<-shellDone
	case <-ctx.Done():
		// The originating request's context ended (server shutdown).
		if cmd.Process != nil {
			killProcessGroup(cmd.Process.Pid)
		}
		conn.Close(websocket.StatusGoingAway, "server shutting down")
		<-shellDone
	}
	_ = ptmx.Close()
	<-outDone
}
