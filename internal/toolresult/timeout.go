package toolresult

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/majorcontext/harness/protocol"
)

var errTimedOut = errors.New("tool call timed out")

// Limit runs call with a context that ends after limit. When the limit ends a
// call that returned an error and the caller's context is still live, the
// result is the error result "<name> timed out after <N> s" and no error.
func Limit(ctx context.Context, name string, limit time.Duration, call func(context.Context) (protocol.ToolResult, error)) (protocol.ToolResult, error) {
	lctx, cancel := context.WithTimeoutCause(ctx, limit, errTimedOut)
	defer cancel()
	res, err := call(lctx)
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(lctx), errTimedOut) {
		return protocol.ToolResult{Text: fmt.Sprintf("%s timed out after %d s", name, int(limit/time.Second)), IsError: true}, nil
	}
	return res, err
}
