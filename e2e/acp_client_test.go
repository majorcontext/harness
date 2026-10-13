package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// acpFrame is one line of the wire. dir is "client" for a line the test wrote
// and "agent" for a line it read.
type acpFrame struct {
	Dir   string          `json:"dir"`
	Frame json.RawMessage `json:"frame"`
}

// acpConn is the test client of one ACP connection. It checks each frame in
// both directions against the pinned schema, so a client that drifts cannot
// hide a decode bug.
type acpConn struct {
	t      *testing.T
	schema *acpSchema
	in     io.Writer
	lines  chan acpLine
	wire   []acpFrame
	nextID int
	// sessions maps a scenario alias to a session ID, dirs a name to a
	// directory, and pending a name to the ID of a request without a response.
	sessions map[string]string
	dirs     map[string]string
	pending  map[string]int
	paths    map[string]string
	fake     *harnesstest.Server
	env      acpEnv
}

type acpLine struct {
	text string
	err  error
}

// acpHost starts one connection to an ACP agent and returns its two pipe ends
// and a stop function that closes the input and waits for the agent to end.
type acpHost struct {
	name  string
	start func(t *testing.T, env acpEnv) (in io.WriteCloser, out io.Reader, stop func())
}

func newACPConn(t *testing.T, schema *acpSchema, in io.Writer, out io.Reader) *acpConn {
	t.Helper()
	c := &acpConn{t: t, schema: schema, in: in, lines: make(chan acpLine, 64),
		sessions: map[string]string{}, dirs: map[string]string{}, pending: map[string]int{}, paths: map[string]string{}}
	go func() {
		r := bufio.NewReader(out)
		for {
			text, err := r.ReadString('\n')
			if text != "" {
				c.lines <- acpLine{text: strings.TrimSuffix(text, "\n")}
			}
			if err != nil {
				c.lines <- acpLine{err: err}
				close(c.lines)
				return
			}
		}
	}()
	return c
}

// raw writes one line as it is. It is the only path that skips the schema.
func (c *acpConn) raw(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatalf("writing to the agent: %v", err)
	}
	c.record("client", line)
}

// send writes a frame after the pinned schema accepts it.
func (c *acpConn) send(frame map[string]any) {
	c.t.Helper()
	if errs := c.schema.frame(false, frame); len(errs) > 0 {
		c.t.Fatalf("the test client built a frame that the schema rejects: %s\n%v", strings.Join(errs, "; "), frame)
	}
	b, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	c.raw(string(b))
}

// request sends a request with the next id and returns that id.
func (c *acpConn) request(method string, params any) int {
	c.t.Helper()
	c.nextID++
	frame := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		frame["params"] = params
	}
	c.send(frame)
	return c.nextID
}

// read returns the next frame of the agent, after the schema accepts it.
func (c *acpConn) read() map[string]any {
	c.t.Helper()
	timer := time.NewTimer(waitBound)
	defer timer.Stop()
	select {
	case l, ok := <-c.lines:
		if !ok || l.err != nil {
			c.t.Fatalf("the agent closed its output while the test waited for a frame: %v", l.err)
		}
		c.t.Logf("DBG %s", l.text)
		return c.decode(l.text)
	case <-timer.C:
		c.t.Fatalf("waited %s for a frame of the agent", waitBound)
	}
	return nil
}

func (c *acpConn) decode(text string) map[string]any {
	c.t.Helper()
	c.record("agent", text)
	var frame map[string]any
	if err := json.Unmarshal([]byte(text), &frame); err != nil {
		c.t.Fatalf("stdout holds a line that is not a JSON object: %q", text)
	}
	if errs := c.schema.frame(true, frame); len(errs) > 0 {
		c.t.Fatalf("the agent wrote a frame that the schema rejects: %s\n%s", strings.Join(errs, "; "), text)
	}
	return frame
}

func (c *acpConn) record(dir, text string) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(text)); err != nil {
		buf.Reset()
		b, _ := json.Marshal(text)
		buf.Write(b)
	}
	c.wire = append(c.wire, acpFrame{Dir: dir, Frame: normalizeACPFrame(buf.Bytes())})
}

// normalizeACPFrame replaces the agent version, which differs between hosts,
// and writes the keys in sorted order.
func normalizeACPFrame(b []byte) json.RawMessage {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	if m, ok := v.(map[string]any); ok {
		if res, ok := m["result"].(map[string]any); ok {
			if info, ok := res["agentInfo"].(map[string]any); ok {
				info["version"] = "<version>"
			}
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

// response reads frames until the response to id and returns it.
func (c *acpConn) response(id int) map[string]any {
	c.t.Helper()
	for {
		f := c.read()
		if _, isMessage := f["method"]; isMessage {
			continue
		}
		if got, ok := f["id"].(float64); ok && int(got) == id {
			return f
		}
		c.t.Fatalf("want the response to request %d, got %v", id, f)
	}
}

// finish closes the input, then checks that the agent wrote nothing but the
// frames that the test read, and that it ended without a bad exit.
func (c *acpConn) finish(stop func()) {
	c.t.Helper()
	if closer, ok := c.in.(io.Closer); ok {
		_ = closer.Close()
	}
	for l := range c.lines {
		if l.err != nil && !errors.Is(l.err, io.EOF) && !errors.Is(l.err, io.ErrClosedPipe) {
			c.t.Errorf("reading the output of the agent: %v", l.err)
		}
		if l.text != "" {
			c.t.Errorf("stdout holds a line that no step read: %q", l.text)
		}
	}
	stop()
}

// startACPProc runs `harness acp` in its own process group.
func startACPProc(t *testing.T) (io.WriteCloser, io.Reader, *procGroup, *lockedBuffer) {
	t.Helper()
	return startACPProcEnv(t, acpEnv{configPath: "/nonexistent/harness-config.json", sessDir: t.TempDir()})
}

// startACPProcEnv is startACPProc with the config and session directory of env.
func startACPProcEnv(t *testing.T, env acpEnv) (io.WriteCloser, io.Reader, *procGroup, *lockedBuffer) {
	t.Helper()
	cmd := exec.Command(harnessBin, "acp")
	cmd.Dir = t.TempDir()
	cmd.Env = cleanEnv(map[string]string{"HARNESS_SESSION_DIR": env.sessDir, "HARNESS_CONFIG": env.configPath, "ANTHROPIC_API_KEY": acpDummyKey})
	return startACPCmd(t, cmd)
}

// startACPCmd runs cmd in its own process group with ACP on its stdin and stdout.
func startACPCmd(t *testing.T, cmd *exec.Cmd) (io.WriteCloser, io.Reader, *procGroup, *lockedBuffer) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &lockedBuffer{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	g := startGroup(t, cmd)
	_ = inR.Close()
	_ = outW.Close()
	return inW, outR, g, stderr
}

// waitACPExit waits for the process to end and fails the test on a bad exit.
func waitACPExit(t *testing.T, g *procGroup, stderr *lockedBuffer) {
	t.Helper()
	timer := time.NewTimer(waitBound)
	defer timer.Stop()
	select {
	case <-g.exited:
	case <-timer.C:
		g.kill()
		t.Errorf("harness acp did not end\nstderr:\n%s", stderr.String())
		return
	}
	if !g.cmd.ProcessState.Success() {
		t.Errorf("harness acp ended with %s\nstderr:\n%s", g.cmd.ProcessState, stderr.String())
	}
}

// startACPBinary runs `harness acp` and returns a stop function that waits for
// it to end after its input closes.
func startACPBinary(t *testing.T, env acpEnv) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	in, out, g, stderr := startACPProcEnv(t, env)
	return in, out, func() { t.Helper(); waitACPExit(t, g, stderr) }
}

func (c *acpConn) mustBeError(f map[string]any, code int) {
	c.t.Helper()
	e, ok := f["error"].(map[string]any)
	if !ok {
		c.t.Fatalf("want an error response, got %v", f)
	}
	if got, _ := e["code"].(float64); int(got) != code {
		c.t.Errorf("error code = %v, want %d (%v)", e["code"], code, e["message"])
	}
}

func (c *acpConn) mustBeResult(f map[string]any, def string) map[string]any {
	c.t.Helper()
	res, ok := f["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("want a result response, got %v", f)
	}
	if errs := c.schema.def(def, res); len(errs) > 0 {
		c.t.Errorf("the result is not a %s: %s", def, fmt.Sprint(errs))
	}
	return res
}
