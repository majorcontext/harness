//go:build js

package jshost

import (
	"bufio"
	"io"
	"strings"
	"sync"
	"syscall/js"
)

// FrameQueue carries the frames from JavaScript to the pipe of the adapter.
// Push never blocks, because the event loop calls it; a goroutine drains the
// queue into the pipe, which blocks until the adapter reads.
type FrameQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	frames []string
	closed bool
}

// NewFrameQueue returns an empty queue.
func NewFrameQueue() *FrameQueue {
	q := &FrameQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Push queues one frame. After close it drops the frame.
func (q *FrameQueue) Push(frame string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.frames = append(q.frames, frame)
	q.cond.Signal()
}

// Close ends the input after the queued frames.
func (q *FrameQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Signal()
}

// Drain writes each frame and a newline to w, and closes w after close. When w
// stops taking input, it drops the rest.
func (q *FrameQueue) Drain(w *io.PipeWriter) {
	for {
		q.mu.Lock()
		for len(q.frames) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.frames) == 0 {
			q.mu.Unlock()
			_ = w.Close()
			return
		}
		frame := q.frames[0]
		q.frames = q.frames[1:]
		q.mu.Unlock()
		if _, err := io.WriteString(w, frame+"\n"); err != nil {
			q.Close()
			_ = w.Close()
			return
		}
	}
}

// PumpFrames calls onFrame with each line of r, without its newline, until r ends.
func PumpFrames(r io.Reader, onFrame js.Value, onErr func(error)) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if cerr := Call(onFrame, strings.TrimSuffix(line, "\n")); cerr != nil {
				onErr(cerr)
			}
		}
		if err != nil {
			return
		}
	}
}

// Call invokes fn with arg. A function that is missing is a no-op, and a
// JavaScript exception is an error, because a panic would end the module.
func Call(fn js.Value, arg string) (err error) {
	if fn.Type() != js.TypeFunction {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(js.Error)
			if !ok {
				panic(r)
			}
			err = ErrorOf(e.Value)
		}
	}()
	fn.Invoke(arg)
	return nil
}

// LogWriter sends each log line to the onLog function of the host.
type LogWriter struct{ Fn js.Value }

func (w LogWriter) Write(p []byte) (int, error) {
	if err := Call(w.Fn, strings.TrimSuffix(string(p), "\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}

// SettleWhen returns a promise that resolves when done closes.
func SettleWhen(done <-chan struct{}) js.Value {
	var executor js.Func
	executor = js.FuncOf(func(_ js.Value, a []js.Value) any {
		resolve := a[0]
		go func() {
			<-done
			resolve.Invoke()
		}()
		go executor.Release()
		return nil
	})
	return js.Global().Get("Promise").New(executor)
}
