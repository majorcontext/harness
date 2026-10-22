package harnesstest

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const codexPath = "/backend-api/codex/responses"

// OpenAIOptions configures NewOpenAI.
type OpenAIOptions struct {
	// APIKey, when set, is the only bearer token the server accepts. A request
	// with another token fails the test.
	APIKey string
	// RefuseWebSocket answers every websocket upgrade with HTTP 426, so a
	// client that prefers websocket must fall back to HTTP.
	RefuseWebSocket bool
	// UncodedChainMiss answers a chain miss with the error the live backend
	// sends: a message that names previous_response_id and carries no code.
	UncodedChainMiss bool
	// Replies adds Codex-only behavior to the Step of the same Name.
	Replies map[string]CodexReply
	// HoldPrewarm makes the server hold the first websocket prewarm without an
	// answer until the client closes its connection.
	HoldPrewarm bool
	// Credentials, when set, replaces APIKey: it returns the label of an accepted
	// bearer token, which the wire events report. Any other token gets HTTP 401,
	// which is no wire-contract violation.
	Credentials func(token string) (label string, ok bool)
	// AccountID, when set with Credentials, is the chatgpt-account-id header that
	// every request must carry, with an originator header.
	AccountID string
	// RedirectTo, when set, answers every request to the Responses path with a 307
	// to this path on the same host. Redirected counts the requests that follow it.
	RedirectTo string
	// Secret, when set, is a value that no request header may hold. SawSecret reports a request that did.
	Secret string
	// Injected makes the server a credential-injecting server in front of the backend: it expects no credential from the
	// client, and records the credential headers of each request in the wire events.
	Injected *InjectedOptions
	// OnReject runs after the server rejects a bearer token, before it answers.
	OnReject func()
	// Models is the catalog that GET /backend-api/codex/models serves. ModelsStatus, when set, answers that request with this HTTP status instead.
	Models       []CodexModel
	ModelsStatus int
}

// InjectedOptions configures the server of an entry with auth "injected".
type InjectedOptions struct {
	// SignedOut makes the server deny every request until SignIn.
	SignedOut bool
	// DenyStatus is the status of a denied request, 401 when zero.
	DenyStatus int
	// WeakProbe answers the model list request with 200 while signed out, as a server does that authorizes the probe by a weaker check than a model call.
	WeakProbe bool
	// ProbeStatus, when set, answers the model list request with this status whatever the sign-in, so a probe is unknown. SetProbeStatus changes it.
	ProbeStatus int
}

// CodexReply is what a Step reply can ask for beyond Reply.
type CodexReply struct {
	// RateLimits reports a subscription usage snapshot: x-codex-* response
	// headers over SSE, a codex.rate_limits frame over websocket.
	RateLimits *RateLimits
	// Reasoning is the summary parts of a reasoning item that the response
	// carries before its text or tool calls, with encrypted content.
	Reasoning []string
	// Forget makes the connection lose the response after it completes, so a
	// request that chains from it gets the chain-miss error.
	Forget bool
	// Drop ends the response after the first text delta, with no terminal
	// event. The Step needs Reply.Text.
	Drop bool
	// Incomplete ends the response with a response.incomplete event that
	// carries this incomplete_details.reason, in place of response.completed.
	Incomplete string
}

// RateLimits is a Codex subscription usage snapshot.
type RateLimits struct {
	Plan               string
	Primary, Secondary *RateWindow
	// Extra holds further windows by limit name, such as "bengalfox-primary". They go out as
	// x-codex-<name>-* headers only; the websocket frame has no such window.
	// The names "primary" and "secondary" belong to the fields above, and an Extra entry of either
	// name is ignored.
	Extra map[string]*RateWindow
}

// RateWindow is one rate-limit window of RateLimits.
type RateWindow struct {
	UsedPercent   float64
	WindowMinutes int64
	ResetAt       int64 // Unix seconds
}

// WireEvent is one transport-level event the OpenAI server saw.
type WireEvent struct {
	Transport string `json:"transport"` // "ws" or "sse"
	// Event is "dial", "refused", "prewarm" (generate false), or "request".
	Event string `json:"event"`
	// Conn is the 1-based websocket connection the event ran on.
	Conn int `json:"conn,omitempty"`
	// ResponsesWebsockets reports the openai-beta responses_websockets header.
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
	// RejectedSchemaKeywords lists the tool-schema keywords the request
	// carries that the live backend refuses.
	RejectedSchemaKeywords []string `json:"rejected_schema_keywords,omitempty"`
	// ReasoningItems counts the reasoning items that the request replays.
	ReasoningItems int `json:"reasoning_items,omitempty"`
	// HostedTools lists the types of the hosted tools of the request, those other than function.
	HostedTools []string `json:"hosted_tools,omitempty"`
	// Credential is the label of the bearer token of the request, from OpenAIOptions.Credentials.
	Credential string `json:"credential,omitempty"`
	// AccountID is the chatgpt-account-id header of the request.
	AccountID string `json:"chatgpt_account_id,omitempty"`
	// Originator is the originator header of the request.
	Originator string `json:"originator,omitempty"`
	// PromptCacheKey is the prompt_cache_key of the request, recorded for an Injected server. It is not in the JSON, because the hash of a session id differs between runs.
	PromptCacheKey string `json:"-"`
	// CredentialHeaders holds the credential headers of the request, by lower-case name, for an Injected server.
	CredentialHeaders map[string]string `json:"credential_headers,omitempty"`
	// SessionHeader reports a session_id header equal to the x-client-request-id header.
	SessionHeader bool `json:"session_header,omitempty"`
	// ItemsWithoutServerID counts the output items that the request sends back (assistant messages,
	// function calls, and reasoning) whose id is not an id that the server gave.
	ItemsWithoutServerID int `json:"items_without_server_id,omitempty"`
}

// stamp copies the identity headers of r into w.
func (o *OpenAI) stamp(w WireEvent, r *http.Request) WireEvent {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if o.opts.Credentials != nil {
		w.Credential, _ = o.opts.Credentials(token)
	}
	w.AccountID = r.Header.Get("chatgpt-account-id")
	w.Originator = r.Header.Get("originator")
	if o.opts.Injected != nil {
		for _, name := range []string{"authorization", "x-api-key", "chatgpt-account-id", "originator"} {
			if v := r.Header.Get(name); v != "" {
				if w.CredentialHeaders == nil {
					w.CredentialHeaders = map[string]string{}
				}
				w.CredentialHeaders[name] = v
			}
		}
	}
	id := r.Header.Get("session_id")
	w.SessionHeader = id != "" && id == r.Header.Get("x-client-request-id")
	return w
}

func newWireEvent(transport, event string, conn int, b openAIBody) WireEvent {
	return WireEvent{
		Transport: transport, Event: event, Conn: conn, PreviousResponseID: b.PreviousResponseID,
		InputItems: len(b.Input), Params: b.params, Include: b.Include,
		ReasoningEffort: b.Reasoning.Effort, ReasoningSummary: b.Reasoning.Summary, ReasoningItems: b.reasoningItems,
		RejectedSchemaKeywords: b.schemaKeywords, HostedTools: b.hostedTools,
	}
}

// OpenAI is a scripted OpenAI Responses server for the ChatGPT Codex wire,
// over SSE and websocket. HoldPrewarm holds the first websocket prewarm, and
// AwaitPrewarmHeld and AwaitPrewarmAbandoned observe it: NewOpenAI rejects Block.
type OpenAI struct {
	*Server
	opts OpenAIOptions

	wmu           sync.Mutex // guards the fields below; never held with Server.mu
	wire          []WireEvent
	modelRequests int
	prewarms      []string // instructions of each websocket prewarm
	conns         int
	responses     map[string]int    // by id prefix
	callNames     map[string]string // call id -> tool name, for chained requests that omit the call
	known         map[string]bool   // "conn/response id" of every completed response
	issued        map[string]bool   // id of every output item the server sent
	faults        []string
	held          bool // a prewarm took the hold of HoldPrewarm

	injectedOut  bool // guarded by wmu
	probeStatus  int  // guarded by wmu
	weakProbe    bool // guarded by wmu
	denials      int  // guarded by wmu
	secretSeen   bool
	rejected     map[string]int // rejected bearer tokens by transport
	redirectHits int            // requests to RedirectTo
	redirectAuth bool           // one of them carried a credential header
}

const heldPrewarm = "prewarm"

// NewOpenAI starts an OpenAI Responses server that matches Steps as NewAnthropic does.
// A websocket prewarm consumes no Step, and a chain to an unknown response
// gets the chain-miss error. Cleanup fails t for a wire-contract violation.
func NewOpenAI(t testing.TB, opts OpenAIOptions, steps ...Step) *OpenAI {
	t.Helper()
	steps = append([]Step(nil), steps...)
	for i := range steps {
		st := &steps[i]
		if r := st.Reply; r.Block || r.StopReason != "" {
			t.Fatalf("harnesstest: step %q: the OpenAI server does not support Block or StopReason", st.Name)
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
		issued:    map[string]bool{},
		Server:    newServer(steps),
	}
	if opts.Injected != nil {
		o.injectedOut, o.probeStatus, o.weakProbe = opts.Injected.SignedOut, opts.Injected.ProbeStatus, opts.Injected.WeakProbe
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

// WireEvents returns a copy of every transport-level event received so far.
func (o *OpenAI) WireEvents() []WireEvent {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return append([]WireEvent(nil), o.wire...)
}

// AwaitPrewarmHeld reports whether the server holds a prewarm within bound.
func (o *OpenAI) AwaitPrewarmHeld(bound time.Duration) bool {
	return o.awaitClosed(o.blockedCh(heldPrewarm), bound)
}

// AwaitPrewarmAbandoned reports whether the client closed the connection of
// the held prewarm within bound. It stays false when server close ended it.
func (o *OpenAI) AwaitPrewarmAbandoned(bound time.Duration) bool {
	return o.awaitClosed(o.canceledCh(heldPrewarm), bound)
}

// holdPrewarm holds the prewarm on conn until its client gives up, and
// reports whether it took the hold.
func (o *OpenAI) holdPrewarm(ctx context.Context, conn *websocket.Conn) bool {
	o.wmu.Lock()
	take := o.opts.HoldPrewarm && !o.held
	o.held = o.held || take
	o.wmu.Unlock()
	if !take {
		return false
	}
	o.mu.Lock()
	closeOnce(o.chanFor(o.blocked, heldPrewarm))
	o.mu.Unlock()
	_, _, err := conn.Read(ctx)
	if err != nil && ctx.Err() == nil {
		o.mu.Lock()
		closeOnce(o.chanFor(o.canceled, heldPrewarm))
		o.mu.Unlock()
	}
	return true
}

// SignIn makes an Injected server accept requests again.
func (o *OpenAI) SignIn() {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.injectedOut = false
}

// SetWeakProbe changes whether an Injected server answers the model list request while it is signed out.
func (o *OpenAI) SetWeakProbe(weak bool) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.weakProbe = weak
}

// SetProbeStatus changes the status that an Injected server gives the model list request. Zero: the sign-in decides.
func (o *OpenAI) SetProbeStatus(status int) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.probeStatus = status
}

// Denials counts the requests that an Injected server denied for want of a sign-in.
func (o *OpenAI) Denials() int {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return o.denials
}

// injectedStatus returns the status that ProbeStatus sets for a model list request, and counts the request as a denial when it does.
func (o *OpenAI) injectedStatus(r *http.Request, peek bool) int {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	if r.URL.Path != codexModelsPath || o.probeStatus == 0 {
		return 0
	}
	if !peek {
		o.denials++
	}
	return o.probeStatus
}

// injectedDenies reports whether an Injected server denies r.
func (o *OpenAI) injectedDenies(r *http.Request) bool {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return o.injectedOut && (!o.weakProbe || r.URL.Path != codexModelsPath)
}

// Rejections counts the requests that Credentials rejected, over the websocket and over SSE.
func (o *OpenAI) Rejections() (ws, sse int) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return o.rejected["ws"], o.rejected["sse"]
}

// SawSecret reports whether a request carried OpenAIOptions.Secret in a header.
func (o *OpenAI) SawSecret() bool {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return o.secretSeen
}

func headersHold(h http.Header, secret string) bool {
	for _, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, secret) {
				return true
			}
		}
	}
	return false
}

// Redirected reports how many requests reached the target of RedirectTo and
// whether one carried an Authorization or chatgpt-account-id header.
func (o *OpenAI) Redirected() (hits int, credentials bool) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return o.redirectHits, o.redirectAuth
}

// PrewarmInstructions returns the instructions of each websocket prewarm, in
// the order they arrived.
func (o *OpenAI) PrewarmInstructions() []string {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	return append([]string(nil), o.prewarms...)
}

func (o *OpenAI) fault(format string, args ...any) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.faults = append(o.faults, fmt.Sprintf(format, args...))
}

func (o *OpenAI) logWire(w WireEvent) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	o.wire = append(o.wire, w)
}

func (o *OpenAI) handle(w http.ResponseWriter, r *http.Request) {
	if o.opts.Secret != "" && headersHold(r.Header, o.opts.Secret) {
		o.wmu.Lock()
		o.secretSeen = true
		o.wmu.Unlock()
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if o.opts.RedirectTo != "" && r.URL.Path == o.opts.RedirectTo {
		o.wmu.Lock()
		o.redirectHits++
		o.redirectAuth = o.redirectAuth || r.Header.Get("Authorization") != "" || r.Header.Get("chatgpt-account-id") != ""
		o.wmu.Unlock()
		writeError(w, http.StatusNotFound, "harnesstest: redirect target")
		return
	}
	if o.opts.RedirectTo != "" && r.URL.Path == codexPath {
		http.Redirect(w, r, o.opts.RedirectTo, http.StatusTemporaryRedirect)
		return
	}
	accepted := false
	if o.opts.Credentials != nil {
		_, accepted = o.opts.Credentials(token)
	}
	switch {
	case r.URL.Path != codexPath && r.URL.Path != codexModelsPath:
		o.fault("request path %q, want %q (from %s, user agent %q)", r.URL.Path, codexPath, r.RemoteAddr, r.UserAgent())
		writeError(w, http.StatusNotFound, "harnesstest: unexpected path")
	case o.opts.Injected != nil && o.injectedStatus(r, true) != 0:
		writeError(w, o.injectedStatus(r, false), "harnesstest: model list unavailable")
	case o.opts.Injected != nil && o.injectedDenies(r):
		o.wmu.Lock()
		o.denials++
		o.wmu.Unlock()
		status := cmp.Or(o.opts.Injected.DenyStatus, http.StatusUnauthorized)
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="harness"`)
		}
		writeError(w, status, "harnesstest: no credential for this caller")
	case o.opts.Credentials != nil && !accepted:
		kind := "sse"
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			kind = "ws"
		}
		o.wmu.Lock()
		if o.rejected == nil {
			o.rejected = map[string]int{}
		}
		o.rejected[kind]++
		o.wmu.Unlock()
		if o.opts.OnReject != nil {
			o.opts.OnReject()
		}
		writeError(w, http.StatusUnauthorized, "harnesstest: token rejected")
	case o.opts.AccountID != "" && (r.Header.Get("chatgpt-account-id") != o.opts.AccountID || r.Header.Get("originator") == ""):
		o.fault("request without the expected chatgpt-account-id and originator headers")
		writeError(w, http.StatusForbidden, "harnesstest: bad account headers")
	case o.opts.Injected == nil && (token == "" || (o.opts.Credentials == nil && o.opts.APIKey != "" && token != o.opts.APIKey)):
		o.fault("request without the expected bearer token")
		writeError(w, http.StatusUnauthorized, "harnesstest: bad bearer token")
	case r.URL.Path == codexModelsPath:
		o.serveModels(w, r)
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
	step := o.steps[i]
	if step.Reply.Calls != nil {
		step.Reply.ToolCalls = step.Reply.Calls(req)
		step.Reply.Calls = nil
	}
	return step, true
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
	wr := o.stamp(newWireEvent("sse", "request", 0, b), r)
	wr.ItemsWithoutServerID = o.itemsWithoutServerID(b)
	if o.opts.Injected != nil {
		wr.PromptCacheKey = b.PromptCacheKey
	}
	wr.ContentEncoding = r.Header.Get("Content-Encoding")
	o.logWire(wr)
	req := o.request(b)
	req.raw = raw
	step, ok := o.pick(req)
	if !ok {
		writeError(w, http.StatusInternalServerError, "harnesstest: no step matched")
		return
	}
	if step.Reply.Hangup {
		panic(http.ErrAbortHandler)
	}
	if step.Reply.HTTPStatus != 0 {
		writeChatReplyError(w, step.Reply)
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
		o.logWire(WireEvent{Transport: "ws", Event: "refused"})
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
	o.logWire(o.stamp(WireEvent{Transport: "ws", Event: "dial", Conn: n,
		ResponsesWebsockets: strings.HasPrefix(r.Header.Get("openai-beta"), "responses_websockets="),
	}, r))
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
	wev := newWireEvent("ws", event, n, b)
	wev.ItemsWithoutServerID = o.itemsWithoutServerID(b)
	if o.opts.Injected != nil {
		wev.PromptCacheKey = b.PromptCacheKey
	}
	o.logWire(wev)
	if b.PreviousResponseID != "" && !o.isKnown(n, b.PreviousResponseID) {
		return writeFrames(ctx, conn, previousResponseNotFound(b.PreviousResponseID, o.opts.UncodedChainMiss))
	}
	if prewarm {
		o.wmu.Lock()
		o.prewarms = append(o.prewarms, b.Instructions)
		o.wmu.Unlock()
		if o.holdPrewarm(ctx, conn) {
			return false
		}
		id, frames := o.prewarmFrames()
		ok := writeFrames(ctx, conn, frames...)
		o.markKnown(n, id)
		return ok
	}
	req := o.request(b)
	req.raw = data
	step, ok := o.pick(req)
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
	if !extra.Forget {
		o.markKnown(n, id)
	}
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

// itemsWithoutServerID counts the output items of b whose id the server did not give.
func (o *OpenAI) itemsWithoutServerID(b openAIBody) int {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	n := 0
	for _, raw := range b.Input {
		var it wireItem
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		output := it.Type == "function_call" || it.Type == "reasoning" || it.Type == "message" && it.Role == "assistant"
		if output && !o.issued[it.ID] {
			n++
		}
	}
	return n
}

// issue records ids as ids of output items that the server gave.
func (o *OpenAI) issue(ids ...string) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	for _, id := range ids {
		o.issued[id] = true
	}
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
