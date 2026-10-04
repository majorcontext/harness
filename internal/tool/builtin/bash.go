package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const (
	bashTimeout = 2 * time.Minute
	// bashOutputCap bounds the output that one command keeps: the head and
	// the tail, around a marker.
	bashOutputCap = 96 * 1024
	// bashWaitDelay bounds the wait for an output pipe that a background
	// child of the command still holds open.
	bashWaitDelay = 2 * time.Second
)

const bashSchema = `{
	"type": "object",
	"properties": {
		"command": {"type": "string", "description": "The shell command to execute"}
	},
	"required": ["command"]
}`

func (d dir) bash() tool {
	return newTool("bash", "Execute a shell command and return its combined stdout and stderr. The command runs with `sh -c` in the session working directory.",
		bashSchema, func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Command == "" {
				return "", errors.New("bash: missing command argument")
			}
			return run(ctx, d.root, in.Command, bashTimeout)
		})
}

// run runs command in its own process group, so a timeout or an interrupt
// kills each child that it put in the background.
func run(ctx context.Context, workDir, command string, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	processGroup(cmd)
	cmd.WaitDelay = bashWaitDelay
	w := newCapped(bashOutputCap)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	text := string(w.Bytes())
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("bash: timeout after %s", timeout)
	case ctx.Err() != nil:
		return "", fmt.Errorf("bash: aborted: %w", ctx.Err())
	case errors.Is(err, exec.ErrWaitDelay):
		return text + "\n[note: a backgrounded process may still be running; its output after this point was not captured]", nil
	case err != nil && text == "":
		return "", errors.New(err.Error())
	case err != nil:
		return "", errors.New(text + "\n" + err.Error())
	}
	return text, nil
}

// capped keeps the first and the last half of a cap of the bytes written,
// so its memory stays bounded however much a command writes.
type capped struct {
	headCap, tailCap int
	head, tail       []byte
	total            int
}

func newCapped(n int) *capped { return &capped{headCap: n / 2, tailCap: n - n/2} }

func (w *capped) Write(p []byte) (int, error) {
	n := len(p)
	w.total += n
	if room := w.headCap - len(w.head); room > 0 {
		room = min(room, len(p))
		w.head = append(w.head, p[:room]...)
		p = p[room:]
	}
	if len(p) > 0 && w.tailCap > 0 {
		w.tail = append(w.tail, p...)
		if len(w.tail) > 2*w.tailCap {
			w.tail = append([]byte(nil), w.tail[len(w.tail)-w.tailCap:]...)
		}
	}
	return n, nil
}

// Bytes returns the output, or its head, a marker that counts the dropped
// bytes, and its tail.
func (w *capped) Bytes() []byte {
	tail := w.tail[max(len(w.tail)-w.tailCap, 0):]
	kept := len(w.head) + len(tail)
	out := append([]byte(nil), w.head...)
	if w.total > kept {
		out = fmt.Appendf(out, "\n... [%d bytes truncated] ...\n", w.total-kept)
	}
	return append(out, tail...)
}
