package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
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

// bashWaitDelay bounds how long cmd.Wait may block on the command's output
// pipes once the underlying process is otherwise done with them. Without it,
// os/exec's internal pipe-copy goroutine (unavoidable because cmd.Stdout
// here is a cappedWriter, not an *os.File) only unblocks Wait on pipe EOF —
// and EOF never arrives if a backgrounded grandchild inherited the write
// end and is still alive, whether because cmd.Cancel couldn't reach it or
// because `sh` simply exited without waiting for it (see cmd.Cancel and the
// exec.ErrWaitDelay handling below, and the issue this fixed). A handful of
// seconds is generous for a killed process tree to actually exit and close
// its fds, while still bounding the worst case to "a few seconds", never
// "forever". Tests override this var to keep wall-clock cost small.
var bashWaitDelay = 5 * time.Second

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

			// Run sh in its own process group so a grandchild that
			// backgrounds itself (`sleep 60 &`, or — the real production
			// trigger — a dev server an agent legitimately starts to test
			// against) can be killed as a unit below. Without this,
			// CommandContext's default Cancel only reaches the direct sh
			// child, orphaning any grandchild that inherited the output
			// pipe's write end and permanently wedging Wait().
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

			// cmd.Cancel runs once cctx is done — BashTimeout elapsing, or
			// the caller aborting the turn (context cancellation). SIGKILL
			// the whole process group (negative pid), not just the direct
			// child, so a backgrounded grandchild dies with it instead of
			// being orphaned holding the pipe open.
			cmd.Cancel = func() error {
				if cmd.Process == nil {
					return os.ErrProcessDone
				}
				if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					return err
				}
				return os.ErrProcessDone
			}

			// See bashWaitDelay: bounds Wait() to a few seconds past
			// whichever comes first — cctx being done, or sh exiting on its
			// own — instead of blocking forever on a pipe a grandchild
			// still holds open.
			cmd.WaitDelay = bashWaitDelay

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

			switch {
			case cctx.Err() == context.DeadlineExceeded:
				// BashTimeout fired; cmd.Cancel has (or is) killing the
				// whole process group. Hand back whatever was captured
				// rather than nothing.
				return message.Parts{&message.Text{Text: text + fmt.Sprintf("\n[command timed out after %s and was killed; output above may be incomplete]", timeout)}}, fmt.Errorf("bash: timeout after %s", timeout)
			case ctx.Err() != nil:
				// The caller's own context was canceled (an aborted turn)
				// ahead of our local timeout. Same shape as a timeout: a
				// clear interruption note plus whatever was captured, not a
				// bare protocol failure.
				return message.Parts{&message.Text{Text: text + "\n[command aborted; output above may be incomplete]"}}, fmt.Errorf("bash: aborted: %w", ctx.Err())
			case errors.Is(err, exec.ErrWaitDelay):
				// sh itself already exited — successfully, since neither of
				// the above fired and cmd.Cancel was therefore never
				// invoked — but a backgrounded grandchild it spawned was
				// still holding the output pipe open when WaitDelay forced
				// it closed. This is the everyday "agent starts a dev
				// server" case: treat it as a successful call (the
				// command's own output was fully captured), not a timeout
				// or an error, with a note that a background process may
				// still be running and producing output we can no longer
				// see.
				return message.Parts{&message.Text{Text: text + "\n[note: a backgrounded process may still be running; its output after this point was not captured]"}}, nil
			case err != nil:
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
