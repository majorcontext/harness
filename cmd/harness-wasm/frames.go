//go:build js

package main

import (
	"bufio"
	"io"
	"strings"
	"sync"
	"syscall/js"
)

// frameQueue carries the frames from JavaScript to the pipe of the adapter.
// push never blocks, because the event loop calls it; a goroutine drains the
// queue into the pipe, which blocks until the adapter reads.
type frameQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	frames []string
	closed bool
}

func newFrameQueue() *frameQueue {
	q := &frameQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push queues one frame. After close it drops the frame.
func (q *frameQueue) push(frame string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.frames = append(q.frames, frame)
	q.cond.Signal()
}

// close ends the input after the queued frames.
func (q *frameQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Signal()
}

// drain writes each frame and a newline to w, and closes w after close. When w
// stops taking input, it drops the rest.
func (q *frameQueue) drain(w *io.PipeWriter) {
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
			q.close()
			_ = w.Close()
			return
		}
	}
}

// pumpFrames calls onFrame with each line of r, without its newline, until r ends.
func pumpFrames(r io.Reader, onFrame js.Value, onErr func(error)) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if cerr := call(onFrame, strings.TrimSuffix(line, "\n")); cerr != nil {
				onErr(cerr)
			}
		}
		if err != nil {
			return
		}
	}
}

// call invokes fn with arg. A function that is missing is a no-op, and a
// JavaScript exception is an error, because a panic would end the module.
func call(fn js.Value, arg string) (err error) {
	if fn.Type() != js.TypeFunction {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(js.Error)
			if !ok {
				panic(r)
			}
			err = errorOf(e.Value)
		}
	}()
	fn.Invoke(arg)
	return nil
}
