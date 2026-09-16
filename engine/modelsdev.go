package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/message"
)

const modelsDevRefreshTTL = time.Hour

const modelsDevMaxBodyBytes = 32 << 20

var modelsDevHTTPClient = &http.Client{Timeout: 5 * time.Second}

func (c Config) modelsDevEnabled() bool {
	return c.ContextWindowFromModelsDev && c.ContextWindowModelsDevURL != ""
}

// modelsDevWindows is the in-memory snapshot of bare model ID -> context
// window tokens. It is replaced whole, never mutated, so the session path
// reads it lock-free.
type modelsDevWindows struct {
	windows map[string]int
}

// modelsDevSnapshot is written only by the background refresher and read by
// resolveContextWindow; the session path therefore performs no network I/O.
var modelsDevSnapshot atomic.Pointer[modelsDevWindows]

// modelsDevWindowLookup resolves ref from the snapshot. A nil snapshot or a
// missing bare ID is a miss.
func modelsDevWindowLookup(ref message.ModelRef) (int, bool) {
	snap := modelsDevSnapshot.Load()
	if snap == nil {
		return 0, false
	}
	tokens, ok := snap.windows[modelsDevModelKey(ref.Model)]
	return tokens, ok
}

// modelsDevContextWindowLookup mirrors modelContextWindowLookup's seam for
// tests that substitute a fixed table.
var modelsDevContextWindowLookup = modelsDevWindowLookup

var modelsDevRefreshMu sync.Mutex

// SetModelsDevRefreshSource names the snapshot URL and launches the
// background refresher. Call it at wiring time; the caller performs no I/O
// and waits for nothing — the first fetch runs inside the goroutine.
// A second call cancels the previous refresher's loop first: two loops
// would race their stores into the snapshot.
func SetModelsDevRefreshSource(ctx context.Context, url string) {
	if url == "" {
		return
	}
	modelsDevRefreshMu.Lock()
	defer modelsDevRefreshMu.Unlock()
	if modelsDevRefreshCancel != nil {
		modelsDevRefreshCancel()
	}
	loopCtx, cancel := context.WithCancel(ctx)
	modelsDevRefreshCancel = cancel
	startModelsDevRefresh(loopCtx, url)
}

var modelsDevRefreshCancel context.CancelFunc

// startModelsDevRefresh launches the one background refresher bound to its
// own source: an immediate fetch inside the goroutine, then the hourly
// ticker. Launched at wiring time, so the request path never launches,
// waits on, or performs I/O — a lookup reads the current snapshot and a
// not-yet-populated one is an ordinary miss.
func startModelsDevRefresh(ctx context.Context, url string) {
	go func() {
		if err := refreshModelsDevWindows(ctx, url); err != nil {
			slog.Warn("engine: models.dev: initial snapshot fetch failed", "error", err.Error())
		}
	}()
	go refreshModelsDevLoop(ctx, url)
}

func refreshModelsDevLoop(ctx context.Context, url string) {
	ticker := time.NewTicker(modelsDevRefreshTTL)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = refreshModelsDevWindows(ctx, url)
		}
	}
}

// refreshModelsDevWindows replaces the snapshot after one success and keeps
// the previous snapshot on a failure.
func refreshModelsDevWindows(ctx context.Context, url string) error {
	windows, err := fetchModelsDevWindows(ctx, url)
	if err != nil {
		return err
	}
	modelsDevSnapshot.Store(&modelsDevWindows{windows: windows})
	return nil
}

func fetchModelsDevWindows(ctx context.Context, url string) (map[string]int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := modelsDevHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("engine: models.dev: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > modelsDevMaxBodyBytes {
		return nil, fmt.Errorf("engine: models.dev: response exceeds %d bytes", modelsDevMaxBodyBytes)
	}
	var raw map[string]*int
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("engine: models.dev: snapshot is null")
	}
	windows := make(map[string]int, len(raw))
	for k, v := range raw {
		if v == nil || *v <= 0 {
			return nil, fmt.Errorf("engine: models.dev: malformed entry for %q", k)
		}
		windows[k] = *v
	}
	return windows, nil
}

// modelsDevModelKey mirrors modelmeta's last-path-segment normalization so a
// Bifrost-routed ref matches the snapshot's bare model IDs.
func modelsDevModelKey(model string) string {
	if idx := strings.LastIndexByte(model, '/'); idx >= 0 {
		return model[idx+1:]
	}
	return model
}
