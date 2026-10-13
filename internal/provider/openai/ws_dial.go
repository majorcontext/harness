//go:build !js

package openai

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
)

const socketAvailable = true

func dialSocket(ctx context.Context, url string, hdr http.Header, httpClient *http.Client) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: httpClient,
		HTTPHeader: hdr,
	})
}
