package claudecode

import (
	"context"
	"errors"
	"sync"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// errAsked ends a drive after a question call opened its request.
var errAsked = errors.New("claudecode: the turn waits for the answer to a question")

// recorded tells the bridge goroutine of a call when the run recorded the call
// as an item, which Sink.Ask needs.
type recorded struct {
	mu    sync.Mutex
	calls map[string]*recordedCall
}

type recordedCall struct {
	once sync.Once
	done chan struct{}
}

func (r *recorded) of(id string) *recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]*recordedCall{}
	}
	c, ok := r.calls[id]
	if !ok {
		c = &recordedCall{done: make(chan struct{})}
		r.calls[id] = c
	}
	return c
}

func (r *recorded) mark(id string) {
	c := r.of(id)
	c.once.Do(func() { close(c.done) })
}

// ask opens the request of a call to the question tool once the run recorded
// the call. It never replies to an opened call: the run interrupts the CLI,
// which records the call as cut off, and the turn that the answer starts sends
// the answer.
func (r *run) ask(ctx context.Context, req turn.Request, c protocol.ToolCall) protocol.ToolResult {
	select {
	case <-r.recorded.of(c.ID).done:
	case <-ctx.Done():
		return protocol.ToolResult{Text: context.Cause(ctx).Error(), IsError: true}
	}
	res, opened := req.Ask(ctx, c)
	if !opened {
		return res
	}
	r.askedCh <- c.ID
	<-ctx.Done()
	return protocol.ToolResult{Text: context.Cause(ctx).Error(), IsError: true}
}

// answered returns the text of the answer that this turn delivers: a turn with
// no input whose history ends with the result of a question call.
func answered(req turn.Request) (string, bool) {
	if len(req.Input) > 0 || len(req.History) == 0 {
		return "", false
	}
	last := req.History[len(req.History)-1]
	if last.Role != protocol.RoleTool {
		return "", false
	}
	for _, p := range last.Parts {
		if p.Type == protocol.LogPartToolResult && p.Name == turn.AskTool && !p.IsError {
			return p.Text, true
		}
	}
	return "", false
}
