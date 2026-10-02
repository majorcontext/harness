package harnesstest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// DefaultCodexPath is the request path NewOpenAI serves unless
// OpenAIOptions.Path says otherwise.
const DefaultCodexPath = "/backend-api/codex/responses"

// OpenAIOptions configures NewOpenAI.
type OpenAIOptions struct {
	// Path is the only request path the server answers. A request for any
	// other path fails the test. The default is DefaultCodexPath.
	Path string
	// APIKey, when set, is the only bearer token the server accepts. A request
	// with another token fails the test.
	APIKey string
	// RefuseWebSocket answers every websocket upgrade with HTTP 426, so a
	// client that prefers websocket must fall back to HTTP.
	RefuseWebSocket bool
	// Replies adds Codex-only behavior to the Step of the same Name.
	Replies map[string]CodexReply
}

// CodexReply is what a Step reply can ask for beyond Reply. The OpenAI server
// does not support Reply.Block, Reply.HTTPStatus, or Reply.StopReason and
// fails the test at construction when a Step sets one.
type CodexReply struct {
	// RateLimits reports a subscription usage snapshot: x-codex-* response
	// headers over SSE, a codex.rate_limits frame over websocket.
	RateLimits *RateLimits
	// Reasoning is the summary parts of a reasoning item that the response
	// carries before its text or tool calls, with encrypted content.
	Reasoning []string
	// Drop ends the response without a terminal event after the first text
	// delta. It closes the websocket, or aborts the SSE response. The Step
	// needs Reply.Text.
	Drop bool
}

// RateLimits is a Codex subscription usage snapshot.
type RateLimits struct {
	Plan               string
	Primary, Secondary *RateWindow
	BengalfoxPrimary   *RateWindow // x-codex-bengalfox-primary-* headers only; the websocket frame has no such window
}

// RateWindow is one rate-limit window of RateLimits.
type RateWindow struct {
	UsedPercent   float64
	WindowMinutes int64
	ResetAt       int64 // Unix seconds
}

// WireRequest is one transport-level event the OpenAI server saw.
type WireRequest struct {
	Transport string `json:"transport"` // "ws" or "sse"
	// Event is "dial" (websocket upgrade accepted), "refused" (upgrade
	// answered with 426), "prewarm" (a response.create with generate false),
	// or "request" (a response that the model scripts).
	Event string `json:"event"`
	// Conn is the 1-based websocket connection the event ran on.
	Conn int `json:"conn,omitempty"`
	// ResponsesWebsockets reports that a dial carried the openai-beta
	// responses_websockets protocol header.
	ResponsesWebsockets bool   `json:"beta_header,omitempty"`
	PreviousResponseID  string `json:"previous_response_id,omitempty"`
	InputItems          int    `json:"input_items,omitempty"`
	// ContentEncoding is the Content-Encoding header of an SSE request.
	ContentEncoding string `json:"content_encoding,omitempty"`
	// Params lists the optional request params present on the wire, from
	// max_output_tokens, temperature, top_p, and metadata.
	Params []string `json:"params,omitempty"`
	// Include is the request include list.
	Include []string `json:"include,omitempty"`
	// ReasoningEffort and ReasoningSummary are the reasoning control of the
	// request.
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	ReasoningSummary string `json:"reasoning_summary,omitempty"`
	// ReasoningItems counts the reasoning items that the request replays.
	ReasoningItems int `json:"reasoning_items,omitempty"`
}

func newWireRequest(transport, event string, conn int, b openAIBody) WireRequest {
	return WireRequest{
		Transport: transport, Event: event, Conn: conn, PreviousResponseID: b.PreviousResponseID,
		InputItems: len(b.Input), Params: b.params, Include: b.Include,
		ReasoningEffort: b.Reasoning.Effort, ReasoningSummary: b.Reasoning.Summary, ReasoningItems: b.reasoningItems,
	}
}

// OpenAI is a scripted OpenAI Responses server that speaks the ChatGPT Codex
// wire over both SSE and websocket. Step, Reply, Matcher, and Request work as
// for New. A websocket response.create with generate false is a prewarm: the
// server answers it without consuming a Step and WireRequests records it.
//
// A request that names a previous_response_id the connection never completed
// gets the previous_response_not_found error frame. Websocket requests that
// chain send only the new input items, so Request.Messages holds only those.
type OpenAI struct {
	*Server
	opts OpenAIOptions

	wmu       sync.Mutex // guards the fields below; never held with Server.mu
	wire      []WireRequest
	conns     int
	responses map[string]int    // by id prefix
	callNames map[string]string // call id -> tool name, for the calls this server issued
	known     map[string]bool   // "conn/response id" of every completed response
	faults    []string
}

// NewOpenAI starts an OpenAI Responses server. It answers each request with
// the first Step, in declaration order, that has not been consumed and whose
// Match accepts it, as New does.
//
// At cleanup NewOpenAI fails t for each unmatched request, each non-Repeat
// Step that never matched, and each request that broke the wire contract: a
// wrong path, a missing bearer token, or an undecodable body.
func NewOpenAI(t testing.TB, opts OpenAIOptions, steps ...Step) *OpenAI {
	t.Helper()
	steps = append([]Step(nil), steps...)
	for i := range steps {
		st := &steps[i]
		if st.Match == nil {
			st.Match = func(Request) bool { return true }
		}
		if r := st.Reply; r.Block || r.HTTPStatus != 0 || r.StopReason != "" {
			t.Fatalf("harnesstest: step %q: the OpenAI server does not support Block, HTTPStatus, or StopReason", st.Name)
		}
		if opts.Replies[st.Name].Drop && st.Reply.Text == "" {
			t.Fatalf("harnesstest: step %q: Drop needs Reply.Text", st.Name)
		}
	}
	o := &OpenAI{
		opts:      opts,
		responses: map[string]int{},
		callNames: map[string]string{},
		known:     map[string]bool{},
		Server: &Server{
			closing:  make(chan struct{}),
			steps:    steps,
			consumed: make([]bool, len(steps)),
			releases: map[string]chan struct{}{},
			blocked:  map[string]chan struct{}{},
			canceled: map[string]chan struct{}{},
			arrived:  make(chan struct{}),
		},
	}
	o.srv = httptest.NewServer(http.HandlerFunc(o.handle))
	t.Cleanup(func() { o.cleanup(t) })
	return o
}

func (o *OpenAI) cleanup(t testing.TB) {
	close(o.closing)
	o.srv.Close()
	o.wmu.Lock()
	for _, f := range o.faults {
		t.Errorf("harnesstest: %s", f)
	}
	o.wmu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	if !t.Failed() {
		for i, st := range o.steps {
			if !st.Repeat && !o.consumed[i] {
				t.Errorf("harnesstest: step %d %q never matched a request", i, st.Name)
			}
		}
	}
	for _, r := range o.unmatched {
		t.Errorf("harnesstest: no step matched request: last user text %q", r.LastUserText())
	}
}

// WireRequests returns a copy of every transport-level event received so far.
func (o *OpenAI) WireRequests() []WireRequest {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return append([]WireRequest(nil), o.wire...)
}

func (o *OpenAI) path() string {
	if o.opts.Path != "" {
		return o.opts.Path
	}
	return DefaultCodexPath
}

func (o *OpenAI) fault(format string, args ...any) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.faults = append(o.faults, fmt.Sprintf(format, args...))
}

func (o *OpenAI) logWire(w WireRequest) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.wire = append(o.wire, w)
}

func (o *OpenAI) handle(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.URL.Path != o.path():
		o.fault("request path %q, want %q", r.URL.Path, o.path())
		writeError(w, http.StatusNotFound, "harnesstest: unexpected path")
	case token == "" || (o.opts.APIKey != "" && token != o.opts.APIKey):
		o.fault("request without the expected bearer token")
		writeError(w, http.StatusUnauthorized, "harnesstest: bad bearer token")
	case strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
		o.serveWebSocket(w, r)
	case r.Method == http.MethodPost:
		o.serveSSE(w, r)
	default:
		o.fault("request method %s", r.Method)
		writeError(w, http.StatusMethodNotAllowed, "harnesstest: unexpected method")
	}
}

// pick records req and returns the Step that answers it.
func (o *OpenAI) pick(req Request) (Step, bool) {
	o.mu.Lock()
	o.requests = append(o.requests, req)
	close(o.arrived)
	o.arrived = make(chan struct{})
	o.mu.Unlock()
	i := o.selectStep(req)
	if i < 0 {
		o.mu.Lock()
		o.unmatched = append(o.unmatched, req)
		o.mu.Unlock()
		return Step{}, false
	}
	return o.steps[i], true
}

func (o *OpenAI) serveSSE(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		raw, err = decodeBodyEncoding(raw, r.Header.Get("Content-Encoding"))
	}
	var b openAIBody
	if err == nil {
		b, err = decodeOpenAIBody(raw)
	}
	if err != nil {
		o.fault("undecodable SSE request: %v", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	wr := newWireRequest("sse", "request", 0, b)
	wr.ContentEncoding = r.Header.Get("Content-Encoding")
	o.logWire(wr)
	step, ok := o.pick(o.request(b))
	if !ok {
		writeError(w, http.StatusInternalServerError, "harnesstest: no step matched")
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no flusher")
		return
	}
	extra := o.opts.Replies[step.Name]
	if extra.RateLimits != nil {
		extra.RateLimits.setHeaders(w.Header())
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	e := sseWriter{w, f}
	_, frames := o.replyFrames(step, false)
	for _, fr := range frames {
		e.event(fr.name, fr.data)
		if extra.Drop && fr.name == frameTextDelta {
			panic(http.ErrAbortHandler)
		}
	}
}

func (o *OpenAI) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	if o.opts.RefuseWebSocket {
		o.logWire(WireRequest{Transport: "ws", Event: "refused"})
		writeError(w, http.StatusUpgradeRequired, "harnesstest: websocket refused")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		o.fault("websocket accept: %v", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(64 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-o.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	n := o.nextConn()
	o.logWire(WireRequest{Transport: "ws", Event: "dial", Conn: n,
		ResponsesWebsockets: strings.HasPrefix(r.Header.Get("openai-beta"), "responses_websockets="),
	})
	for {
		_, data, err := conn.Read(ctx)
		if err != nil || !o.serveCreate(ctx, conn, n, data) {
			return
		}
	}
}

func (o *OpenAI) nextConn() int {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.conns++
	return o.conns
}

// serveCreate answers one response.create frame. It reports whether the
// connection stays open.
func (o *OpenAI) serveCreate(ctx context.Context, conn *websocket.Conn, n int, data []byte) bool {
	b, err := decodeOpenAIBody(data)
	if err == nil && b.Type != "response.create" {
		err = fmt.Errorf("frame type %q, want response.create", b.Type)
	}
	if err != nil {
		o.fault("undecodable websocket frame: %v", err)
		return false
	}
	prewarm := b.Generate != nil && !*b.Generate
	event := "request"
	if prewarm {
		event = "prewarm"
	}
	o.logWire(newWireRequest("ws", event, n, b))
	if b.PreviousResponseID != "" && !o.isKnown(n, b.PreviousResponseID) {
		return writeFrames(ctx, conn, previousResponseNotFound(b.PreviousResponseID))
	}
	if prewarm {
		id, frames := o.prewarmFrames()
		ok := writeFrames(ctx, conn, frames...)
		o.markKnown(n, id)
		return ok
	}
	step, ok := o.pick(o.request(b))
	if !ok {
		return writeFrames(ctx, conn, newFrame("error", obj{"error": obj{"type": "server_error", "message": "harnesstest: no step matched"}}))
	}
	extra := o.opts.Replies[step.Name]
	id, frames := o.replyFrames(step, true)
	for _, fr := range frames {
		if !writeFrames(ctx, conn, fr) {
			return false
		}
		if extra.Drop && fr.name == frameTextDelta {
			_ = conn.CloseNow()
			return false
		}
	}
	o.markKnown(n, id)
	return true
}

func writeFrames(ctx context.Context, conn *websocket.Conn, frames ...frame) bool {
	for _, fr := range frames {
		b, _ := json.Marshal(fr.data)
		if conn.Write(ctx, websocket.MessageText, b) != nil {
			return false
		}
	}
	return true
}

func (o *OpenAI) isKnown(conn int, id string) bool {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return o.known[fmt.Sprintf("%d/%s", conn, id)]
}

func (o *OpenAI) markKnown(conn int, id string) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.known[fmt.Sprintf("%d/%s", conn, id)] = true
}
