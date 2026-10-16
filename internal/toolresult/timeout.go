package toolresult

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/majorcontext/harness/protocol"
)

var errTimedOut = errors.New("tool call timed out")

// Limit runs call with a context that ends after limit. When the limit ends
// first, or ends a call that returned an error, and the caller's context is
// still live, the result is the error result "<name> timed out after <N> s"
// and no error. The result does not wait for a call that ignores its context.
func Limit(ctx context.Context, name string, limit time.Duration, call func(context.Context) (protocol.ToolResult, error)) (protocol.ToolResult, error) {
	lctx, cancel := context.WithTimeoutCause(ctx, limit, errTimedOut)
	defer cancel()
	type outcome struct {
		res protocol.ToolResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := call(lctx)
		done <- outcome{res, err}
	}()
	timedOut := protocol.ToolResult{Text: fmt.Sprintf("%s timed out after %d s", name, int(limit/time.Second)), IsError: true}
	select {
	case o := <-done:
		if o.err != nil && ctx.Err() == nil && errors.Is(context.Cause(lctx), errTimedOut) {
			return timedOut, nil
		}
		return o.res, o.err
	case <-lctx.Done():
		if ctx.Err() != nil {
			o := <-done
			return o.res, o.err
		}
		return timedOut, nil
	}
}
