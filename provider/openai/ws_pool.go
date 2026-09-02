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

// Default pool tunables, ported from opencode's ws-pool.ts
// (DEFAULT_CONNECT_TIMEOUT/DEFAULT_IDLE_TIMEOUT/DEFAULT_MAX_CONNECTION_AGE)
// and its streamRetries default.
const (
	wsDefaultConnectTimeout   = 15 * time.Second
	wsDefaultIdleTimeout      = 5 * time.Minute
	wsDefaultMaxConnectionAge = 55 * time.Minute
	wsDefaultStreamRetries    = 5
)

// errStreamClosedEarly marks a websocket stream torn down by its own
// caller (Close called before a terminal event) rather than by a wire-level
// failure. It is never returned to a caller of Client.Stream; it only
// drives wsPool's failure bookkeeping (see wsFrameSource.close).
var errStreamClosedEarly = errors.New("openai: websocket stream closed before a terminal event")

type wsLineage struct {
	request     *apiRequest
	responseID  string
	outputItems []json.RawMessage
	generation  uint64
}

// wsPoolEntry is one pooled session's websocket state — the Go analog of
// opencode's ws-pool.ts PoolEntry. This session's next request reuses conn
// as long as it is still open, younger than maxConnectionAge, and the
// session has not been marked fallback/busy.
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
	// subUsage is the subscription-usage snapshot captured off conn's own
	// dial (upgrade response) headers — see codexSubscriptionUsageFromHeaders
	// and dialResponsesWebSocket's doc comment. Only ever set for a
	// CodexFamily request (see stream below); nil otherwise.
	//
	// KNOWN STALENESS: the Codex backend sends these headers on the
	// websocket upgrade response only, never on any later frame, so this
	// is refreshed exclusively when conn itself is re-dialed — NOT on
	// every turn a reused connection serves. On an actively-reused
	// connection, the value can therefore lag the account's real usage by
	// up to the connection's own reuse window: idleTimeout (5 minutes of
	// no traffic invalidates it) or maxConnectionAge (55 minutes, whichever
	// comes first — see wsPool's own tunables above). Contrast the HTTP
	// path (Client.codexSubscriptionUsage), which re-reads fresh headers
	// on every single request with no such lag. This is a deliberate,
	// accepted limitation, not a bug: a caller showing this value (e.g.
	// boxes rendering it in a UI) also has SubscriptionUsage.CapturedAt,
	// so staleness is always visible rather than silently assumed live.
	// No periodic redial or separate query exists to force a refresh.
	subUsage *message.SubscriptionUsage
}

// wsPool is a per-Client pool of persistent Codex Responses websocket
// connections, one per harness session (keyed by provider.Request.
// SessionKey), reused across turns. It is the Go port of opencode's
// ws-pool.ts createWebSocketFetch, adapted to provider.Provider's
// Stream/Next shape instead of a fetch() interception.
//
// Every failure mode falls back to the caller using HTTP for that request:
// wsPool.stream's second return value is false whenever the caller must not
// use the (nil) stream it returned — including "did not even attempt ws".
// Nothing here can make a request WORSE than the pre-existing HTTP path.
type wsPool struct {
	connectTimeout   time.Duration
	idleTimeout      time.Duration
	maxConnectionAge time.Duration
	streamRetries    int

	// dial is overridden by tests that point it at an httptest server
	// instead of the real chatgpt.com endpoint. Production always uses
	// dialResponsesWebSocket via newWSPool's default assignment.
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

// entryFor returns the pool entry for sessionKey, creating one on first
// use. The key mirrors opencode's `${sessionID}:conversation` — the suffix
// exists there to leave room for a second, differently-scoped key on the
// same session (e.g. title generation); harness has no such second use of
// this adapter yet, but the suffix costs nothing and keeps the port
// literal.
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

// wsStreamRequest is the input wsPool.stream needs beyond the pool's own
// tunables — everything Client.Stream already computed for the HTTP path,
// so the ws path sends the byte-identical wire request.
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

// stream attempts to serve req over this pool's session-affine websocket,
// returning (nil, false) whenever the caller must fall back to HTTP instead
// — a busy or permanently-fallen-back session, a dial failure, a send
// failure, or a failure to read even the first response frame. A non-nil
// stream is only ever returned once at least one real response frame has
// been read successfully, mirroring opencode's onFirstEvent gate: a socket
// that dies before producing anything must never be handed to the engine as
// if it were a working stream, since the engine has no transport-level
// retry of its own to fall back to HTTP with (Next() failing mid-response
// is reported as a truncated stream, not silently retried elsewhere — see
// provider.Stream's doc comment).
func (p *wsPool) stream(ctx context.Context, req wsStreamRequest) (provider.Stream, bool) {
	entry := p.entryFor(req.SessionKey)

	entry.mu.Lock()
	if entry.fallback || entry.busy {
		// A competing request falls back to HTTP. Invalidate the generation so
		// the in-flight WebSocket completion cannot publish stale lineage.
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
		// Only a CodexFamily request captures the x-codex-* subscription-
		// usage headers off the upgrade response — see CodexFamily's own
		// doc comment for why family is the gate, mirroring Client.
		// codexSubscriptionUsage's identical check on the HTTP path.
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
	newSource := func(name string, data []byte) *wsFrameSource {
		return &wsFrameSource{
			ctx:         ctx,
			conn:        conn,
			idleTimeout: p.idleTimeout,
			buffered:    &wsFrame{name: name, data: data},
			onTerminal: func(name string, data []byte, first bool) {
				// Keep only a first-frame chain miss on the socket until stream.Next
				// replaces it with the immutable complete request below.
				if first && chainedRequest && !recoveryAttempted && isPreviousResponseNotFoundFrame(name, data) {
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
	if chainedRequest {
		st.recoverChainMiss = func(first bool, visible bool, chainErr error) (*wsFrameSource, *provider.RequestMetadata, error) {
			recoveryAttempted = true
			p.clearLineage(entry, generation)
			if !first || visible {
				p.invalidate(entry)
				p.release(entry)
				if visible {
					return nil, nil, provider.MarkStreamTruncated(chainErr)
				}
				return nil, nil, chainErr
			}
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
