//go:build js

package openai

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"
)

// A browser WebSocket cannot set request headers, so the Codex websocket
// transport cannot dial on js.
func dialSocket(context.Context, string, http.Header, *http.Client) (*websocket.Conn, *http.Response, error) {
	return nil, nil, errors.New("websocket transport unavailable on js")
}
