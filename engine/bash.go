package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// defaultBashOutputCap is the fallback for Config.BashOutputCap: the maximum
// number of bytes of combined stdout+stderr kept from one bash call before it
// is truncated. 96KB is generous enough for ordinary command output (test
// runs, git logs, file dumps) while bounding the worst case — an apt-get or
// npm install storm that would otherwise dump megabytes into a single
// message, bloating the session log and the next provider request built from
// it (see AGENTS.md and docs/goal-loop.md for the incident this fixed).
const defaultBashOutputCap = 96 * 1024

func bashTool(timeout time.Duration, outputCap int) Tool {
	if outputCap <= 0 {
		outputCap = defaultBashOutputCap
	}
	return Tool{
		Def: provider.ToolDef{
			Name:        "bash",
			Description: "Execute a shell command and return its combined stdout and stderr. The command runs with `sh -c` in the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "The shell command to execute"}
				},
				"required": ["command"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Command == "" {
				return nil, fmt.Errorf("bash: missing command argument")
			}

			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := exec.CommandContext(cctx, "sh", "-c", in.Command)
			cmd.Dir = s.cfg.WorkDir
			cmd.Env = os.Environ()
			for k, v := range s.shellEnv(ctx, "bash", in.Command) {
				cmd.Env = append(cmd.Env, k+"="+v)
			}

			// cappedWriter bounds memory to O(outputCap) regardless of how
			// much the command actually emits — the truncation happens
			// during capture, not after buffering everything, so a runaway
			// command cannot balloon this process's memory before its
			// output ever reaches the message log (see cappedWriter).
			cw := newCappedWriter(outputCap)
			cmd.Stdout = cw
			cmd.Stderr = cw
			err := cmd.Run()

			text := string(cw.Bytes())

			if cctx.Err() == context.DeadlineExceeded {
				return message.Parts{&message.Text{Text: text + fmt.Sprintf("\n[command timed out after %s]", timeout)}}, fmt.Errorf("bash: timeout after %s", timeout)
			}
			if err != nil {
				if text == "" {
					text = err.Error()
				} else {
					text += "\n" + err.Error()
				}
				return nil, fmt.Errorf("%s", text)
			}
			return message.Parts{&message.Text{Text: text}}, nil
		},
	}
}

// cappedWriter is an io.Writer that captures at most a bounded amount of a
// stream's head and tail, no matter how much is written through it. It
// exists so one bash command emitting gigabytes of output (an apt-get or npm
// install storm is the real-world trigger) allocates only O(cap) memory —
// never the full stream — before its output is truncated for the session
// log and the next provider request.
//
// The budget is split evenly between a head buffer (fixed once full) and a
// tail buffer (a trimmed sliding window of the most recent bytes). Bytes.
// reconstructs "first half of the cap" + a truncation marker naming exactly
// how many bytes were dropped + "second half of the cap" — never neither
// end, so an error banner at the very end of an apt-get log (the usual
// reason to look at output at all) is always visible in the tail, and the
// command being run is visible in the head.
type cappedWriter struct {
	headCap int
	tailCap int
	head    []byte // grows only until it reaches headCap, then is left alone
	tail    []byte // a trimmed sliding window of up to 2*tailCap bytes
	total   int    // total bytes ever written, truncated or not
}

func newCappedWriter(cap int) *cappedWriter {
	headCap := cap / 2
	return &cappedWriter{headCap: headCap, tailCap: cap - headCap}
}

// Write never fails: it always reports len(p) written, matching the
// best-effort nature of output capture (dropping bytes past the cap is the
// point, not an error).
func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.total += n

	if len(w.head) < w.headCap {
		room := w.headCap - len(w.head)
		if room > len(p) {
			room = len(p)
		}
		w.head = append(w.head, p[:room]...)
		p = p[room:]
	}

	if len(p) > 0 && w.tailCap > 0 {
		w.tail = append(w.tail, p...)
		// Compact only occasionally (once the buffer grows to twice the
		// cap) so this stays amortized O(n) instead of trimming on every
		// write call.
		if len(w.tail) > w.tailCap*2 {
			trimmed := make([]byte, w.tailCap)
			copy(trimmed, w.tail[len(w.tail)-w.tailCap:])
			w.tail = trimmed
		}
	}
	return n, nil
}

// Bytes returns the captured output: untouched if it never exceeded the cap,
// otherwise head + a "N bytes truncated" marker + the most recent tailCap
// bytes.
func (w *cappedWriter) Bytes() []byte {
	tail := w.tail
	if len(tail) > w.tailCap {
		tail = tail[len(tail)-w.tailCap:]
	}
	kept := len(w.head) + len(tail)
	if w.total <= kept {
		out := make([]byte, 0, kept)
		out = append(out, w.head...)
		out = append(out, tail...)
		return out
	}
	dropped := w.total - kept
	marker := fmt.Sprintf("\n... [%d bytes truncated] ...\n", dropped)
	out := make([]byte, 0, len(w.head)+len(marker)+len(tail))
	out = append(out, w.head...)
	out = append(out, marker...)
	out = append(out, tail...)
	return out
}
