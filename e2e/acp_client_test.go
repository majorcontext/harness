package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	// reconnect ends the agent process and starts a new one over the same session directory.
	reconnect func()
	// scanned is the index in wire up to which await steps have taken frames.
	scanned int
	// cursor is the nextCursor of the last session/list.
	cursor string
	// asked maps a name to the id of an elicitation/create request of the agent.
	asked map[string]any
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
	c := &acpConn{t: t, schema: schema,
		sessions: map[string]string{}, dirs: map[string]string{}, pending: map[string]int{}, paths: map[string]string{}}
	c.attach(in, out)
	return c
}

// attach reads the agent output out and writes to in. A reconnect attaches the
// pipes of a new agent process to the same connection record.
func (c *acpConn) attach(in io.Writer, out io.Reader) {
	lines := make(chan acpLine, 64)
	c.in, c.lines = in, lines
	go func() {
		r := bufio.NewReader(out)
		for {
			text, err := r.ReadString('\n')
			if text != "" {
				lines <- acpLine{text: strings.TrimSuffix(text, "\n")}
			}
			if err != nil {
				lines <- acpLine{err: err}
				close(lines)
				return
			}
		}
	}()
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
	timer := time.NewTimer(finishBound)
	defer timer.Stop()
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				stop()
				return
			}
			if l.err != nil && !errors.Is(l.err, io.EOF) && !errors.Is(l.err, io.ErrClosedPipe) {
				c.t.Errorf("reading the output of the agent: %v", l.err)
			}
			if l.text != "" {
				c.t.Errorf("stdout holds a line that no step read: %q", l.text)
			}
		case <-timer.C:
			c.t.Fatalf("the agent did not close its output within %s after its input closed%s", finishBound, stallReport(c.t))
		}
	}
}

// finishBound bounds the wait for the output of an agent to close after its
// input closes. A healthy agent needs milliseconds.
const finishBound = 20 * time.Second

// rowBound is the hard limit of one Node host run.
const rowBound = 45 * time.Second

// stallDumps maps a test to a function that asks its agent process for a state
// report, kills the process, and returns the report with the stderr.
var stallDumps sync.Map

// stallReport returns the state report of the agent of t, or an empty string.
func stallReport(t *testing.T) string {
	fn, ok := stallDumps.Load(t)
	if !ok {
		return ""
	}
	return "\n" + fn.(func() string)()
}

// drain closes the input and fails on any line that no step read, until the
// agent closes its output.
func (c *acpConn) drain() {
	c.t.Helper()
	if closer, ok := c.in.(io.Closer); ok {
		_ = closer.Close()
	}
	for l := range c.lines {
		if l.text != "" {
			c.t.Errorf("stdout holds a line that no step read: %q", l.text)
		}
	}
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
	vars := map[string]string{"HARNESS_SESSION_DIR": env.sessDir, "HARNESS_CONFIG": env.configPath, "ANTHROPIC_API_KEY": acpDummyKey, "PATH": pathWithoutClaude()}
	if env.login != nil {
		maps.Copy(vars, env.login.procEnv())
		cmd.Stderr = env.login.sink
	}
	cmd.Env = cleanEnv(vars)
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
	cmd.Stdin, cmd.Stdout = inR, outW
	if cmd.Stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, cmd.Stderr)
	} else {
		cmd.Stderr = stderr
	}
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
