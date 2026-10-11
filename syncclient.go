package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

const (
	syncPostTimeout = time.Minute
)

// httpSync posts each SyncBatch to a control plane. The reply is a SyncAck.
// A 409 with protocol.CodeStaleEpoch is ErrStaleEpoch, and one with
// protocol.CodeSyncConflict is ErrConflict. A 400 with invalid_request is
// ErrInvalidRequest, and any 413, 401, or 403 is ErrSyncRejected, whatever its
// code. All of them are final. Any other failure is retried by the sender.
type httpSync struct {
	url       string
	tokenFile string
	client    *http.Client
}

func newHTTPSync(s config.SyncSpec) *httpSync {
	return &httpSync{url: s.URL, tokenFile: s.TokenFile, client: &http.Client{Timeout: syncPostTimeout}}
}

// Deliver reads the token again for each post, so a rotated token takes effect.
func (h *httpSync) Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error) {
	token, err := os.ReadFile(h.tokenFile)
	if err != nil {
		return protocol.SyncAck{}, fmt.Errorf("harness: sync token: %w", err)
	}
	body, err := json.Marshal(b)
	if err != nil {
		return protocol.SyncAck{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return protocol.SyncAck{}, errors.New("harness: sync request is not valid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := h.client.Do(req)
	if err != nil {
		return protocol.SyncAck{}, fmt.Errorf("harness: sync post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var e protocol.ErrorBody
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return protocol.SyncAck{}, rejection(resp.StatusCode, e.Error.Code, b)
	}
	var ack protocol.SyncAck
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return protocol.SyncAck{}, fmt.Errorf("harness: sync reply: %w", err)
	}
	return ack, nil
}

// rejection maps a failed post to its error. Only the final statuses of the
// receiver contract get a sentinel; the sender resends a batch for any other.
func rejection(status int, code string, b protocol.SyncBatch) error {
	switch {
	case status == http.StatusConflict && code == protocol.CodeStaleEpoch:
		return fmt.Errorf("%w: epoch %d", ErrStaleEpoch, b.Epoch)
	case status == http.StatusConflict && code == protocol.CodeSyncConflict:
		return fmt.Errorf("%w: the receiver holds other bytes at seq %d", ErrConflict, b.FromSeq)
	case status == http.StatusBadRequest && code == protocol.CodeInvalidRequest:
		return fmt.Errorf("%w: %w: seq %d", ErrInvalidRequest, ErrSyncRejected, b.FromSeq)
	case status == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: seq %d is over the size bound of the receiver", ErrSyncRejected, b.FromSeq)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: status %d", ErrSyncRejected, status)
	}
	return fmt.Errorf("harness: sync post: status %d %s", status, code)
}
