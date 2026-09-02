package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// canned ws response.* frames for one complete, tool-free turn — the ws
// analog of streamFixture in stream_test.go, minus the SSE "event:"
// envelope (a ws frame carries only the JSON body; its own "type" field is
// the event name — see readResponsesFrame).
var wsCannedFrames = []string{
	`{"type":"response.created","response":{"id":"resp_ws_1"}}`,
	`{"type":"response.output_text.delta","output_index":0,"delta":"hi"}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_ws_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`,
	`{"type":"response.completed","response":{"id":"resp_ws_1","usage":{"input_tokens":5,"output_tokens":2}}}`,
}

// wsFrameEventName extracts a canned frame's "type" field so tests can
// serve the identical fixture over both transports: unwrapped over ws (see
// readResponsesFrame), and under an SSE "event:" line for the HTTP+SSE
// fallback path (see stream.readSSE), which key on it differently.
func wsFrameEventName(frame string) string {
	var env wsFrameEnvelope
	json.Unmarshal([]byte(frame), &env) //nolint:errcheck
	return env.Type
}

// wsTestServer is an httptest server that speaks BOTH halves of this
// adapter's wire: a normal HTTP+SSE Responses POST, and (on an Upgrade
// request) the Codex response.create websocket protocol, replaying
// wsCannedFrames for every response.create it reads on a connection —
// enough to prove pool reuse sends more than one turn over the same
// accepted connection.
type wsTestServer struct {
	*httptest.Server
	upgrades   atomic.Int32
	lastAuth   atomic.Value // string
	rejectWS   atomic.Bool
	closeAfter atomic.Bool // close the connection after one response instead of looping
}

func newWSTestServer(t *testing.T) *wsTestServer {
	t.Helper()
	ts := &wsTestServer{}
	ts.lastAuth.Store("")
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.lastAuth.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Upgrade") == "" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, f := range wsCannedFrames {
				io.WriteString(w, sse(wsFrameEventName(f), f)) //nolint:errcheck
			}
			return
		}
		if ts.rejectWS.Load() {
			http.Error(w, "websocket disabled", http.StatusForbidden)
			return
		}
		ts.upgrades.Add(1)
		// InsecureSkipVerify here disables coder/websocket's Origin-header
		// check for this loopback httptest server (plain HTTP, no TLS
		// involved) — it is not a TLS setting and does not weaken
		// certificate verification anywhere in this transport.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusInternalError, "")
		for {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			_, _, err := conn.Read(ctx)
			cancel()
			if err != nil {
				return
			}
			for _, f := range wsCannedFrames {
				if err := conn.Write(context.Background(), websocket.MessageText, []byte(f)); err != nil {
					return
				}
			}
			if ts.closeAfter.Load() {
				conn.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func wsRequest(sessionKey string) *provider.Request {
	return &provider.Request{
		Model:      message.ModelRef{Provider: Family, Model: "gpt-5"},
		Messages:   []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
		MaxTokens:  10,
		SessionKey: sessionKey,
	}
}

// TestWebSocketTransportConnectsAndStreams: the happy path end to end —
// UseWebSocketTransport + a SessionKey routes Client.Stream over the ws
// server, sends response.create, and the resulting provider.Stream carries
// the same assembled message/usage the HTTP+SSE path would for identical
// wire events (stream.handle is shared code — see openai.go's readEvent).
func TestWebSocketTransportConnectsAndStreams(t *testing.T) {
	ts := newWSTestServer(t)
	c := &Client{APIKey: "test-key", BaseURL: ts.URL, UseWebSocketTransport: true}

	s, err := c.Stream(context.Background(), wsRequest("sess-1"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	events := collect(t, s)

	if ts.upgrades.Load() != 1 {
		t.Fatalf("upgrades = %d, want 1 (must have used the websocket path)", ts.upgrades.Load())
	}
	if got := ts.lastAuth.Load().(string); got != "Bearer test-key" {
		t.Errorf("Authorization sent over ws = %q, want Bearer test-key", got)
	}

	var text string
	var done *provider.Event
	for i := range events {
		if events[i].Type == provider.EventTextDelta {
			text += events[i].Text
		}
		if events[i].Type == provider.EventDone {
			done = &events[i]
		}
	}
	if text != "hi" {
		t.Errorf("text = %q, want hi", text)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done.StopReason != provider.StopEndTurn {
		t.Errorf("stop reason = %s, want end_turn", done.StopReason)
	}
	if done.Usage.InputTokens != 5 || done.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", done.Usage)
	}
	if done.Message == nil || done.Message.ID != "resp_ws_1" {
		t.Errorf("message = %+v", done.Message)
	}
}

// TestWebSocketTransportSendsResponseCreate proves the frame this transport
// puts on the wire is {"type":"response.create", ...request-minus-stream}
// — the exact framing ws.ts's streamResponsesWebSocket uses, and NOT the
// bare Responses request body the HTTP path POSTs.
func TestWebSocketTransportSendsResponseCreate(t *testing.T) {
	var gotType string
	var gotStreamPresent bool
	done := make(chan struct{})

	inspecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_, data, err := conn.Read(r.Context())
		if err != nil {
			close(done)
			return
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(data, &fields) //nolint:errcheck
		if v, ok := fields["type"]; ok {
			json.Unmarshal(v, &gotType) //nolint:errcheck
		}
		_, gotStreamPresent = fields["stream"]
		conn.Write(context.Background(), websocket.MessageText, []byte(wsCannedFrames[len(wsCannedFrames)-1])) //nolint:errcheck
		close(done)
	}))
	defer inspecting.Close()
	c := &Client{APIKey: "k", BaseURL: inspecting.URL, UseWebSocketTransport: true}

	s, err := c.Stream(context.Background(), wsRequest("sess-frame"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	collect(t, s)
	<-done

	if gotType != "response.create" {
		t.Errorf("frame type = %q, want response.create", gotType)
	}
	if gotStreamPresent {
		t.Error("response.create frame still carries \"stream\" — must be stripped (meaningless once ws IS the stream)")
	}
}

// TestWebSocketTransportFallbackOnNoSessionKey: UseWebSocketTransport is on
// but the request carries no SessionKey — there is no pool key, so the
// call must go straight to HTTP without even attempting a dial.
func TestWebSocketTransportFallbackOnNoSessionKey(t *testing.T) {
	ts := newWSTestServer(t)
	c := &Client{APIKey: "k", BaseURL: ts.URL, UseWebSocketTransport: true}

	s, err := c.Stream(context.Background(), wsRequest(""))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	collect(t, s)

	if ts.upgrades.Load() != 0 {
		t.Errorf("upgrades = %d, want 0 (must not attempt ws with no session key)", ts.upgrades.Load())
	}
}

// TestWebSocketTransportFallbackOnDialFailure: a server that refuses the
// upgrade handshake (a proxy/box without ws support, or the backend
// rejecting it) must still let the turn complete over HTTP — the whole
// point of the fallback design.
func TestWebSocketTransportFallbackOnDialFailure(t *testing.T) {
	ts := newWSTestServer(t)
	ts.rejectWS.Store(true)
	c := &Client{APIKey: "k", BaseURL: ts.URL, UseWebSocketTransport: true}

	s, err := c.Stream(context.Background(), wsRequest("sess-2"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	events := collect(t, s)

	if ts.upgrades.Load() != 0 {
		t.Errorf("upgrades = %d, want 0", ts.upgrades.Load())
	}
	var sawDone bool
	for _, ev := range events {
		if ev.Type == provider.EventDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Error("no done event: HTTP fallback did not complete the turn")
	}
}

// TestWebSocketTransportDisabledUsesHTTP: the default (false) must never
// attempt a dial at all, byte-identical to this adapter's pre-existing
// behavior.
func TestWebSocketTransportDisabledUsesHTTP(t *testing.T) {
	ts := newWSTestServer(t)
	c := &Client{APIKey: "k", BaseURL: ts.URL} // UseWebSocketTransport left false

	s, err := c.Stream(context.Background(), wsRequest("sess-3"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	collect(t, s)

	if ts.upgrades.Load() != 0 {
		t.Errorf("upgrades = %d, want 0 (transport is off)", ts.upgrades.Load())
	}
}

// TestWebSocketTransportPoolReusesConnection: two turns on the same
// session must share one accepted websocket connection, not dial twice.
func TestWebSocketTransportPoolReusesConnection(t *testing.T) {
	ts := newWSTestServer(t)
	c := &Client{APIKey: "k", BaseURL: ts.URL, UseWebSocketTransport: true}

	for i := 0; i < 2; i++ {
		s, err := c.Stream(context.Background(), wsRequest("sess-reuse"))
		if err != nil {
			t.Fatalf("Stream #%d: %v", i, err)
		}
		collect(t, s)
		s.Close()
	}

	if got := ts.upgrades.Load(); got != 1 {
		t.Errorf("upgrades = %d, want 1 (second turn should reuse the pooled connection)", got)
	}
}

// TestWebSocketTransportPoolDropsConnectionAfterFailedResponse: a terminal
// event other than response.completed (here, response.failed) must not
// leave its connection pooled for reuse — ported from opencode's
// ws-pool.ts onTerminal, which invalidates on anything but completed/done.
func TestWebSocketTransportPoolDropsConnectionAfterFailedResponse(t *testing.T) {
	failThenSucceed := []string{
		`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`,
	}
	var upgrades atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		for _, f := range failThenSucceed {
			conn.Write(context.Background(), websocket.MessageText, []byte(f)) //nolint:errcheck
		}
	}))
	defer srv.Close()
	c := &Client{APIKey: "k", BaseURL: srv.URL, UseWebSocketTransport: true}

	for i := 0; i < 2; i++ {
		s, err := c.Stream(context.Background(), wsRequest("sess-drop"))
		if err != nil {
			t.Fatalf("Stream #%d: %v", i, err)
		}
		for {
			_, err := s.Next()
			if err != nil {
				break // response.failed surfaces as a stream error, not io.EOF
			}
		}
		s.Close()
	}

	if got := upgrades.Load(); got != 2 {
		t.Errorf("upgrades = %d, want 2 (a failed response must not be pooled for reuse)", got)
	}
}

// TestWebSocketTransportBusyFallsBackToHTTP: a second concurrent call on a
// session whose socket is already mid-turn must use HTTP rather than
// contend for the same connection.
func TestWebSocketTransportBusyFallsBackToHTTP(t *testing.T) {
	release := make(chan struct{})
	var upgrades atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, f := range wsCannedFrames {
				io.WriteString(w, sse(wsFrameEventName(f), f)) //nolint:errcheck
			}
			return
		}
		upgrades.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		<-release // hold the connection "busy" until the test releases it
		for _, f := range wsCannedFrames {
			conn.Write(context.Background(), websocket.MessageText, []byte(f)) //nolint:errcheck
		}
	}))
	defer srv.Close()
	c := &Client{APIKey: "k", BaseURL: srv.URL, UseWebSocketTransport: true}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		s, err := c.Stream(context.Background(), wsRequest("sess-busy"))
		if err != nil {
			return
		}
		defer s.Close()
		collect(t, s)
	}()

	// Give the first call time to reach the pool's busy state before firing
	// the second one — bounded by the "polling for a condition" pattern
	// this codebase's own retry/backoff tests use rather than a raw sleep
	// standing in for synchronization.
	deadline := time.After(2 * time.Second)
	for upgrades.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("first call never reached the ws server")
		case <-time.After(time.Millisecond):
		}
	}

	s, err := c.Stream(context.Background(), wsRequest("sess-busy"))
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	collect(t, s)
	s.Close()
	close(release)
	<-firstDone

	if got := upgrades.Load(); got != 1 {
		t.Errorf("upgrades = %d, want 1 (the busy session's second call must use HTTP)", got)
	}
}

// TestWebSocketTransportMessageTooBigPermanentFallback: a 1009 close marks
// the session permanently HTTP-only, not just for the failed attempt —
// ported from opencode's ws-pool.ts fallback-on-MESSAGE_TOO_BIG.
func TestWebSocketTransportMessageTooBigPermanentFallback(t *testing.T) {
	var upgrades atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, f := range wsCannedFrames {
				io.WriteString(w, sse(wsFrameEventName(f), f)) //nolint:errcheck
			}
			return
		}
		upgrades.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		conn.Close(websocket.StatusMessageTooBig, "message too big")
	}))
	defer srv.Close()
	c := &Client{APIKey: "k", BaseURL: srv.URL, UseWebSocketTransport: true}

	for i := 0; i < 2; i++ {
		s, err := c.Stream(context.Background(), wsRequest("sess-too-big"))
		if err != nil {
			t.Fatalf("Stream #%d: %v", i, err)
		}
		collect(t, s)
		s.Close()
	}

	if got := upgrades.Load(); got != 1 {
		t.Errorf("upgrades = %d, want 1 (MESSAGE_TOO_BIG must permanently fall back to HTTP for this session)", got)
	}
}

// TestToWebSocketURL checks the http(s)->ws(s) rewrite ws.go's dial uses.
func TestToWebSocketURL(t *testing.T) {
	cases := map[string]string{
		"https://chatgpt.com/backend-api/codex/responses": "wss://chatgpt.com/backend-api/codex/responses",
		"http://localhost:1234/v1/responses":              "ws://localhost:1234/v1/responses",
	}
	for in, want := range cases {
		if got := toWebSocketURL(in); got != want {
			t.Errorf("toWebSocketURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsWSTerminalEvent/TestIsWSCleanTerminalEvent lock in the event-name
// classification wsPool's onTerminal/onBroken wiring depends on.
func TestIsWSTerminalEvent(t *testing.T) {
	for _, name := range []string{"response.completed", "response.done", "response.incomplete", "response.failed", "error"} {
		if !isWSTerminalEvent(name) {
			t.Errorf("isWSTerminalEvent(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"response.created", "response.output_text.delta", ""} {
		if isWSTerminalEvent(name) {
			t.Errorf("isWSTerminalEvent(%q) = true, want false", name)
		}
	}
}

func TestIsWSCleanTerminalEvent(t *testing.T) {
	for _, name := range []string{"response.completed", "response.done"} {
		if !isWSCleanTerminalEvent(name) {
			t.Errorf("isWSCleanTerminalEvent(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"response.incomplete", "response.failed", "error"} {
		if isWSCleanTerminalEvent(name) {
			t.Errorf("isWSCleanTerminalEvent(%q) = true, want false", name)
		}
	}
}

func TestWebSocketIncompleteAndFailedResponsesClearLineage(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"response.incomplete","response":{"id":"resp_bad","incomplete_details":{"reason":"max_output_tokens"}}}`,
		`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`,
	} {
		t.Run(wsFrameEventName(terminal), func(t *testing.T) {
			server := newWSLineageServer(t)
			server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_one", "two")}
			server.scripts <- wsLineageScript{beforeWait: []string{terminal}}
			server.scripts <- wsLineageScript{beforeWait: completedLineageFrames("resp_three", "six")}
			client := &Client{APIKey: "test", BaseURL: server.URL, Family: CodexFamily, UseWebSocketTransport: true}

			streamLineageTurn(t, client, lineageRequest("bad-"+wsFrameEventName(terminal), userMessage("one")))
			stream, err := client.Stream(context.Background(), lineageRequest("bad-"+wsFrameEventName(terminal), userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three")))
			if err != nil {
				t.Fatalf("second Stream: %v", err)
			}
			for {
				_, err = stream.Next()
				if err != nil {
					break
				}
			}
			_ = stream.Close()
			streamLineageTurn(t, client, lineageRequest("bad-"+wsFrameEventName(terminal), userMessage("one"), assistantMessage("resp_one", "two"), userMessage("three"), assistantMessage("resp_bad", "four"), userMessage("five")))

			<-server.frames
			<-server.frames
			third := decodeResponseCreate(t, <-server.frames)
			if third.PreviousResponseID != "" || len(third.Input) != 5 {
				t.Fatalf("third frame retained bad lineage: previous=%q input=%s", third.PreviousResponseID, third.Input)
			}
		})
	}
}
