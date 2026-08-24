package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// StdioTransport configures a client that spawns an MCP server as a child
// process and speaks newline-delimited JSON-RPC over its stdin/stdout, per
// https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#stdio.
type StdioTransport struct {
	// Command is the argv of the server process; Command[0] is resolved via
	// PATH like os/exec.Command.
	Command []string
	// Env is appended to the current process's environment.
	Env []string
	// Dir is the child process's working directory; empty uses the
	// current one.
	Dir string

	// dial overrides process spawning; used by tests to run a fake server
	// in-process over a net.Pipe, the same pattern plugin/plugin_test.go
	// uses for its fake plugins.
	dial func() (io.ReadWriteCloser, error)
}

func (t *StdioTransport) open(onNotify notificationHandler) (transport, error) {
	rwc, err := t.dialFunc()
	if err != nil {
		return nil, err
	}
	handler := func(_ context.Context, method string, params json.RawMessage) (any, error) {
		// This client implements no server-initiated methods (no roots,
		// sampling, or elicitation): notifications are logged and
		// continued; requests get a method-not-found error.
		onNotify(method, params)
		return nil, &RPCError{Code: codeMethodNotFound, Message: fmt.Sprintf("unsupported method %q", method)}
	}
	c := newConn(rwc, handler)
	go c.run() //nolint:errcheck // stream end surfaces via pending calls
	return &stdioTransport{c: c}, nil
}

func (t *StdioTransport) dialFunc() (io.ReadWriteCloser, error) {
	if t.dial != nil {
		return t.dial()
	}
	if len(t.Command) == 0 {
		return nil, fmt.Errorf("mcp: stdio transport: no command configured")
	}
	cmd := exec.Command(t.Command[0], t.Command[1:]...)
	cmd.Dir = t.Dir
	cmd.Env = append(os.Environ(), t.Env...)
	cmd.Stderr = os.Stderr // server logging; never interpreted as protocol data.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procConn{stdin: stdin, stdout: stdout, cmd: cmd}, nil
}

// stdioTransport adapts conn (the duplex JSON-RPC connection) to the
// transport interface.
type stdioTransport struct {
	c *conn
}

func (t *stdioTransport) call(ctx context.Context, method string, params, result any) error {
	return t.c.call(ctx, method, params, result)
}

func (t *stdioTransport) notify(ctx context.Context, method string, params any) error {
	return t.c.notify(ctx, method, params)
}

func (t *stdioTransport) close() error {
	return t.c.close()
}

// procConn adapts a child process's stdin/stdout to an io.ReadWriteCloser,
// implementing the shutdown sequence the spec recommends for stdio: close
// stdin, wait for exit, SIGTERM if it doesn't, then SIGKILL if it still
// doesn't
// (https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle#stdio).
type procConn struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func (p *procConn) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *procConn) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *procConn) Close() error {
	_ = p.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()

	graceful := time.NewTimer(2 * time.Second)
	defer graceful.Stop()
	select {
	case <-done:
		return nil
	case <-graceful.C:
	}

	_ = p.cmd.Process.Signal(os.Interrupt) // best-effort SIGTERM equivalent
	term := time.NewTimer(2 * time.Second)
	defer term.Stop()
	select {
	case <-done:
		return nil
	case <-term.C:
	}

	_ = p.cmd.Process.Kill()
	<-done
	return nil
}
