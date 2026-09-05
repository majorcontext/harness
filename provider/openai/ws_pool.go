package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// Default WebSocket pool limits.
const (
	wsDefaultConnectTimeout   = 15 * time.Second
	wsDefaultIdleTimeout      = 5 * time.Minute
	wsDefaultMaxConnectionAge = 55 * time.Minute
	wsDefaultStreamRetries    = 5
)

// errStreamClosedEarly marks a caller that closes a stream before completion.
var errStreamClosedEarly = errors.New("openai: websocket stream closed before a terminal event")

type wsLineage struct {
	request     *apiRequest
	responseID  string
	outputItems []json.RawMessage
	generation  uint64
}

// wsPoolEntry holds one session's WebSocket state.
type wsPoolEntry struct {
	mu             sync.Mutex
	conn           *websocket.Conn
	connectedAt    time.Time
	lastUsedAt     time.Time
	busy           bool
	fallback       bool // permanent: this session never uses ws again
	streamFailures int
	generation     uint64
	lineage        *wsLineage
	// subUsage is captured during the WebSocket upgrade and can be stale.
	subUsage *message.SubscriptionUsage
}

// wsPool reuses a persistent Codex Responses WebSocket for each session.
// It returns false when the caller must fall back to HTTP.
type wsPool struct {
	connectTimeout   time.Duration
	idleTimeout      time.Duration
	maxConnectionAge time.Duration
	streamRetries    int

	// dial is replaced in tests.
	dial func(ctx context.Context, url string, headers http.Header, httpClient *http.Client, timeout time.Duration) (*websocket.Conn, *http.Response, error)

	mu      sync.Mutex
	entries map[string]*wsPoolEntry
}

func newWSPool() *wsPool {
	return &wsPool{
		connectTimeout:   wsDefaultConnectTimeout,
		idleTimeout:      wsDefaultIdleTimeout,
		maxConnectionAge: wsDefaultMaxConnectionAge,
		streamRetries:    wsDefaultStreamRetries,
		dial:             dialResponsesWebSocket,
		entries:          make(map[string]*wsPoolEntry),
	}
}

// entryFor returns the pool entry for sessionKey, creating it when needed.
func (p *wsPool) entryFor(sessionKey string) *wsPoolEntry {
	key := sessionKey + ":conversation"
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if !ok {
		e = &wsPoolEntry{}
		p.entries[key] = e
	}
	return e
}

// wsStreamRequest contains the request data for wsPool.stream.
type wsStreamRequest struct {
	SessionKey string
	URL        string
	Headers    http.Header
	Body       []byte // the marshaled apiRequest, same bytes the HTTP path POSTs
	Model      message.ModelRef
	Family     string
	HTTPClient *http.Client
	Prewarm    bool
}

// stream returns a session WebSocket stream after it reads its first event.
// It returns false when the caller must fall back to HTTP.
func (p *wsPool) stream(ctx context.Context, req wsStreamRequest) (provider.Stream, bool) {
	entry := p.entryFor(req.SessionKey)

	entry.mu.Lock()
	if entry.fallback || entry.busy {
		// A competing request invalidates lineage before it falls back to HTTP.
		entry.lineage = nil
		entry.generation++
		entry.mu.Unlock()
		return nil, false
	}
	entry.busy = true
	now := time.Now()
	reuse := entry.conn != nil &&
		!entry.connectedAt.IsZero() &&
		now.Sub(entry.connectedAt) < p.maxConnectionAge &&
		now.Sub(entry.lastUsedAt) < p.idleTimeout
	entry.lastUsedAt = now
	conn := entry.conn
	entry.mu.Unlock()

	var subUsage *message.SubscriptionUsage
	if !reuse {
		p.invalidate(entry)
		newConn, resp, err := p.dial(ctx, req.URL, req.Headers, req.HTTPClient, p.connectTimeout)
		if err != nil {
			p.recordFailure(entry)
			p.release(entry)
			return nil, false
		}
		// Only Codex-family requests use subscription usage headers.
		if req.Family == CodexFamily && resp != nil {
			subUsage = codexSubscriptionUsageFromHeaders(resp.Header)
		}
		entry.mu.Lock()
		entry.conn = newConn
		entry.connectedAt = time.Now()
		entry.subUsage = subUsage
		entry.lineage = nil
		entry.generation++
		entry.mu.Unlock()
		conn = newConn
	} else {
		entry.mu.Lock()
		subUsage = entry.subUsage
		entry.mu.Unlock()
	}

	var completeRequest apiRequest
	if err := json.Unmarshal(req.Body, &completeRequest); err != nil {
		p.invalidate(entry)
		p.release(entry)
		return nil, false
	}

	var createOptions responseCreateOptions
	entry.mu.Lock()
	generation := entry.generation
	if req.Prewarm {
		generate := false
		completeRequest.Input = make([]json.RawMessage, 0)
		createOptions.Input = completeRequest.Input
		createOptions.InputSet = true
		createOptions.Generate = &generate
	} else if req.Family == CodexFamily && entry.lineage != nil &&
		entry.lineage.responseID != "" &&
		entry.lineage.generation == generation &&
		responsesRequestPropertiesMatch(entry.lineage.request, &completeRequest) {
		if suffix, ok := incrementalInput(entry.lineage.request, entry.lineage.outputItems, completeRequest.Input); ok {
			createOptions.PreviousResponseID = entry.lineage.responseID
			createOptions.Input = suffix
			createOptions.InputSet = true
		}
	}
	entry.mu.Unlock()

	if err := sendResponseCreate(ctx, conn, req.Body, createOptions); err != nil {
		p.handleTransportError(entry, err)
		p.release(entry)
		return nil, false
	}

	firstName, firstData, err := readFirstFrame(ctx, conn, p.idleTimeout)
	if err != nil {
		p.handleTransportError(entry, err)
		p.release(entry)
		return nil, false
	}

	recoveryAttempted := false
	chainedRequest := createOptions.PreviousResponseID != ""
	// recoverable gates chain-miss recovery beyond an explicit
	// previous_response_id: a REUSED pooled connection can carry the
	// server's own implicit session/conversation state even when this
	// particular request is already a complete, non-chained one (for
	// example the first request after a model switch, which
	// responsesRequestPropertiesMatch already refuses to chain). A
	// not-found on that connection is still recoverable. A brand-new dial
	// serving a non-chained request has nothing stale to recover from, so
	// its not-found is a genuine error.
	recoverable := !req.Prewarm && (chainedRequest || reuse)
	newSource := func(name string, data []byte) *wsFrameSource {
		return &wsFrameSource{
			ctx:         ctx,
			conn:        conn,
			idleTimeout: p.idleTimeout,
			buffered:    &wsFrame{name: name, data: data},
			onTerminal: func(name string, data []byte, first bool) {
				// Keep only a first-frame chain miss on the socket until stream.Next
				// replaces it with the immutable complete request below.
				if first && recoverable && !recoveryAttempted && isPreviousResponseNotFoundFrame(name, data) {
					return
				}
				entry.mu.Lock()
				entry.busy = false
				entry.lastUsedAt = time.Now()
				entry.streamFailures = 0
				keep := isWSCleanTerminalEvent(name)
				entry.mu.Unlock()
				if !keep {
					p.invalidate(entry)
				}
			},
			onBroken: func(err error) {
				p.release(entry)
				if errors.Is(err, errStreamClosedEarly) {
					p.invalidate(entry)
					return
				}
				p.handleTransportError(entry, err)
			},
		}
	}

	metadata := &provider.RequestMetadata{
		Mode:                 provider.RequestModeFull,
		CompleteInputItems:   len(completeRequest.Input),
		SentInputItems:       len(completeRequest.Input),
		PreviousResponseUsed: false,
	}
	if chainedRequest {
		metadata.Mode = provider.RequestModeIncremental
		metadata.SentInputItems = len(createOptions.Input)
		metadata.PreviousResponseUsed = true
	}

	st := &stream{
		wsConn:           newSource(firstName, firstData),
		model:            req.Model,
		family:           req.Family,
		subUsage:         subUsage,
		requestMetadata:  metadata,
		recoverChainMiss: nil,
		onComplete: func(responseID string, assistant *message.Message) {
			if req.Family != CodexFamily {
				return
			}
			if responseID == "" {
				p.clearLineage(entry, generation)
				return
			}
			var outputItems []json.RawMessage
			if !req.Prewarm {
				var err error
				outputItems, err = transcodeMessage(assistant, false, req.Family)
				if err != nil {
					p.clearLineage(entry, generation)
					return
				}
			}
			if outputItems == nil {
				outputItems = make([]json.RawMessage, 0)
			}
			entry.mu.Lock()
			defer entry.mu.Unlock()
			if entry.generation != generation {
				return
			}
			entry.lineage = &wsLineage{
				request:     &completeRequest,
				responseID:  responseID,
				outputItems: outputItems,
				generation:  generation,
			}
		},
	}
	if recoverable {
		st.recoverChainMiss = func(first bool, visible bool, chainErr error) (*wsFrameSource, *provider.RequestMetadata, error) {
			recoveryAttempted = true
			if !first || visible {
				// wsFrameSource.onTerminal already released and invalidated this
				// non-first error. Repeating that cleanup here can race with and
				// close a newer request's connection for the same session.
				if visible {
					return nil, nil, provider.MarkStreamTruncated(chainErr)
				}
				return nil, nil, chainErr
			}
			// The server just rejected this socket's conversation/response
			// reference — whether we explicitly sent one (previous_response_id)
			// or the connection was reused and carried the server's own implicit
			// session state. Resending on the SAME socket risks the server
			// tying the rejection to the connection itself, not only to one
			// response ID, so drop this session's entire pooled connection and
			// lineage and dial a genuinely new one before retrying — the same
			// generation-bump-twice pattern stream() itself uses on a non-reuse
			// dial, so a concurrent invalidation cannot resurrect stale state.
			p.invalidate(entry)
			newConn, dialResp, dialErr := p.dial(ctx, req.URL, req.Headers, req.HTTPClient, p.connectTimeout)
			if dialErr != nil {
				p.recordFailure(entry)
				p.release(entry)
				return nil, nil, provider.MarkStreamTruncated(dialErr)
			}
			entry.mu.Lock()
			entry.conn = newConn
			entry.connectedAt = time.Now()
			if req.Family == CodexFamily && dialResp != nil {
				entry.subUsage = codexSubscriptionUsageFromHeaders(dialResp.Header)
			}
			entry.lineage = nil
			entry.generation++
			generation = entry.generation
			entry.mu.Unlock()
			conn = newConn
			if err := sendResponseCreate(ctx, conn, req.Body); err != nil {
				p.handleTransportError(entry, err)
				p.release(entry)
				return nil, nil, provider.MarkStreamTruncated(err)
			}
			name, data, err := readFirstFrame(ctx, conn, p.idleTimeout)
			if err != nil {
				p.handleTransportError(entry, err)
				p.release(entry)
				return nil, nil, provider.MarkStreamTruncated(err)
			}
			fullMetadata := &provider.RequestMetadata{
				Mode:                 provider.RequestModeFull,
				CompleteInputItems:   len(completeRequest.Input),
				SentInputItems:       len(completeRequest.Input),
				PreviousResponseUsed: false,
				ChainRecovered:       true,
			}
			return newSource(name, data), fullMetadata, nil
		}
	}
	return st, true
}

func readFirstFrame(ctx context.Context, conn *websocket.Conn, idleTimeout time.Duration) (string, []byte, error) {
	if idleTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, idleTimeout)
		defer cancel()
	}
	return readResponsesFrame(ctx, conn)
}

// handleTransportError applies a real connection-level failure to entry:
// permanent fallback for a MESSAGE_TOO_BIG close (ported from opencode's
// ws-pool.ts onConnectionInvalid), otherwise a counted failure toward
// streamRetries. Either way the (now-suspect) connection is dropped.
func (p *wsPool) handleTransportError(entry *wsPoolEntry, err error) {
	if websocket.CloseStatus(err) == wsMessageTooBigCode {
		entry.mu.Lock()
		entry.fallback = true
		entry.mu.Unlock()
		p.invalidate(entry)
		return
	}
	p.recordFailure(entry)
}

// recordFailure counts a connection failure toward this entry's
// permanent-fallback threshold and drops its (now-suspect) connection.
// Mirrors opencode's ws-pool.ts recordStreamFailure: Codex counts retries
// AFTER the initial failed attempt, so streamRetries+1 total attempts are
// allowed before an entry gives up on ws for the rest of the session.
func (p *wsPool) recordFailure(entry *wsPoolEntry) {
	entry.mu.Lock()
	entry.streamFailures++
	if entry.streamFailures > p.streamRetries {
		entry.fallback = true
	}
	entry.mu.Unlock()
	p.invalidate(entry)
}

// release clears the busy flag a failed attempt set, without touching
// fallback/streamFailures (recordFailure/handleTransportError own those) —
// called on every exit path that must not leave an entry permanently
// marked busy.
func (p *wsPool) release(entry *wsPoolEntry) {
	entry.mu.Lock()
	entry.busy = false
	entry.lastUsedAt = time.Now()
	entry.mu.Unlock()
}

func (p *wsPool) clearLineage(entry *wsPoolEntry, generation uint64) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.generation == generation {
		entry.lineage = nil
	}
}

// invalidate closes and clears entry's connection, if any, so the next
// stream() call for this session dials fresh. Closing is best-effort: the
// connection is already known bad, terminal, or about to be discarded
// either way.
func (p *wsPool) invalidate(entry *wsPoolEntry) {
	entry.mu.Lock()
	conn := entry.conn
	entry.conn = nil
	entry.connectedAt = time.Time{}
	entry.lineage = nil
	entry.generation++
	entry.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}
