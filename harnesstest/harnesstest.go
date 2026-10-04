// Package harnesstest provides scripted Anthropic Messages, OpenAI
// chat-completions, and OpenAI Responses servers for tests.
package harnesstest

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
)

// Step is one scripted reply and the requests it answers.
type Step struct {
	Name   string  // key for Release; names the step in failure messages
	Match  Matcher // nil matches every request
	Reply  Reply
	Repeat bool // stays available after it matches, and may go unused
}

// Reply is what the server sends when a Step matches.
type Reply struct {
	Text       string
	ToolCalls  []ToolCall // emitted as tool_use blocks after Text
	StopReason string     // default "end_turn", or "tool_use" when ToolCalls is set; "max_tokens" ends the turn at the output limit
	Usage      Usage      // default {Input: 5, Output: 3}; Input past a context-window threshold triggers auto-compaction
	HTTPStatus int        // non-zero: reply with this status and an Anthropic error body
	// ErrorMessage is the message of the error body of an HTTPStatus reply.
	// The default is "harnesstest: scripted error".
	ErrorMessage string
	// RetryAfter is the Retry-After header value of an HTTPStatus reply.
	RetryAfter string
	Block      bool // after the first content delta, wait for Release or client cancel; a Repeat step blocks only until its first Release
	// Reasoning is streamed before Text as reasoning_content deltas. Only the
	// chat-completions server (NewChat) sends it.
	Reasoning string
	// ErrorCode is the error.code of an HTTPStatus reply, for example
	// "context_length_exceeded". Only NewChat sends it.
	ErrorCode string
}

// ContextOverflowMessage is the error message Anthropic sends for a prompt
// over the context window. Use it as Reply.ErrorMessage with HTTPStatus 400.
const ContextOverflowMessage = "prompt is too long: 205102 tokens > 200000 maximum"

// ToolCall is a tool_use block in a Reply.
type ToolCall struct {
	ID, Name string
	Input    map[string]any
}

// Usage is the token usage a Reply reports.
type Usage struct{ Input, Output int }

// Request is a decoded model request.
type Request struct {
	System         string
	Model          string
	MaxTokens      int // the response cap; 0 when the request sends none
	ThinkingType   string
	ThinkingBudget int
	ServiceTier    string
	Messages       []Message
	Tools          []string // sorted names
	// Header holds the HTTP request headers. Only NewChat fills it.
	Header http.Header
	// Chat-completions requests only.
	ReasoningEffort string // top-level reasoning_effort
	User            string // top-level user
	PromptCacheKey  string // top-level prompt_cache_key
}

// Message is one conversation turn in a Request.
type Message struct {
	Role  string
	Parts []Part
}

// Part is one content block of a Message.
type Part struct {
	Kind      string // "text" | "tool_use" | "tool_result" | "image" (chat wire only; Text is its URL)
	Text      string
	ToolName  string
	ToolInput map[string]any
	ToolUseID string
	IsError   bool
}

// LastUserText is the text of the last user message that has a text part.
// A message made only of engine context does not count: the chat wire sends
// it as its own user message, where the Anthropic wire folds it into the
// previous one.
func (r Request) LastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != "user" {
			continue
		}
		var texts []string
		engineOnly := true
		for _, p := range r.Messages[i].Parts {
			if p.Kind == "text" {
				texts = append(texts, p.Text)
				engineOnly = engineOnly && strings.HasPrefix(p.Text, message.EngineContextOpenTag)
			}
		}
		if len(texts) > 0 && !engineOnly {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

// codec is the wire format of a Server.
type codec struct {
	decode     func(body []byte, h http.Header) (Request, error)
	stream     func(s *Server, w http.ResponseWriter, r *http.Request, n int, name string, rep Reply)
	writeError func(w http.ResponseWriter, status int, msg string)
	replyError func(w http.ResponseWriter, rep Reply)
	// pathSuffix, when set, is the end of the only path the server answers.
	pathSuffix string
}

var anthropicCodec = codec{
	decode: func(body []byte, _ http.Header) (Request, error) { return decodeRequest(body) },
	stream: (*Server).stream, writeError: writeError, replyError: writeReplyError,
}

func (s *Server) wire() codec {
	if s.codec == nil {
		return anthropicCodec
	}
	return *s.codec
}

// Server is a scripted model server.
type Server struct {
	codec   *codec // nil speaks the Anthropic wire
	srv     *httptest.Server
	closing chan struct{}

	selMu     sync.Mutex // serializes step selection; held while Match runs, never with mu
	mu        sync.Mutex
	steps     []Step
	consumed  []bool
	requests  []Request
	unmatched []Request
	undecoded []string
	releases  map[string]chan struct{}
	blocked   map[string]chan struct{}
	canceled  map[string]chan struct{}
	arrived   chan struct{} // closed and replaced when a request is recorded
}

func newServer(steps []Step) *Server {
	steps = append([]Step(nil), steps...)
	for i := range steps {
		if steps[i].Match == nil {
			steps[i].Match = func(Request) bool { return true }
		}
	}
	return &Server{
		closing:  make(chan struct{}),
		steps:    steps,
		consumed: make([]bool, len(steps)),
		releases: map[string]chan struct{}{},
		blocked:  map[string]chan struct{}{},
		canceled: map[string]chan struct{}{},
		arrived:  make(chan struct{}),
	}
}

// New starts a Server that answers each request with the first step, in
// declaration order, that has not been consumed and whose Match accepts it.
// A step without Repeat is consumed when it matches.
//
// A request that no step matches gets HTTP 500. A request whose body does
// not decode gets HTTP 400. At cleanup New fails t for each such
// request and for each non-Repeat step that never matched, unless t has
// already failed.
func New(t testing.TB, steps ...Step) *Server {
	t.Helper()
	return start(t, anthropicCodec, steps)
}

func start(t testing.TB, c codec, steps []Step) *Server {
	t.Helper()
	s := newServer(steps)
	s.codec = &c
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() {
		close(s.closing)
		s.srv.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, msg := range s.undecoded {
			t.Errorf("harnesstest: undecodable request body: %s", msg)
		}
		if !t.Failed() {
			for i, st := range s.steps {
				if !st.Repeat && !s.consumed[i] {
					t.Errorf("harnesstest: step %d %q never matched a request", i, st.Name)
				}
			}
		}
		for _, r := range s.unmatched {
			sys := r.System
			if len(sys) > 80 {
				sys = sys[:80]
			}
			t.Errorf("harnesstest: no step matched request: last user text %q, system prefix %q", r.LastUserText(), sys)
		}
	})
	return s
}

// URL is the base URL of the server.
func (s *Server) URL() string { return s.srv.URL }

// Requests returns a copy of every request received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Release lets the named Block step finish. A Release before the request arrives still counts.
func (s *Server) Release(stepName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	closeOnce(s.chanFor(s.releases, stepName))
}

// blockedCh is closed once a request for the named Block step is waiting.
func (s *Server) blockedCh(stepName string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chanFor(s.blocked, stepName)
}

// canceledCh is closed once the client has dropped a waiting request for the named Block step.
func (s *Server) canceledCh(stepName string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chanFor(s.canceled, stepName)
}

// AwaitCanceled reports whether the client dropped a waiting request for the
// named Block step within bound. It stays false for a request that Release
// or server close ended.
func (s *Server) AwaitCanceled(stepName string, bound time.Duration) bool {
	return s.awaitClosed(s.canceledCh(stepName), bound)
}

func (s *Server) awaitClosed(ch <-chan struct{}, bound time.Duration) bool {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	case <-s.closing:
		return false
	}
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// AwaitRequests reports whether the server has received at least n requests
// within bound. It reads the recorded total, so it does not depend on the
// order in which concurrent handlers finish.
func (s *Server) AwaitRequests(n int, bound time.Duration) bool {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for {
		s.mu.Lock()
		got, arrived := len(s.requests), s.arrived
		s.mu.Unlock()
		if got >= n {
			return true
		}
		select {
		case <-arrived:
		case <-timer.C:
			return false
		case <-s.closing:
			return false
		}
	}
}

func (s *Server) chanFor(m map[string]chan struct{}, name string) chan struct{} {
	ch, ok := m[name]
	if !ok {
		ch = make(chan struct{})
		m[name] = ch
	}
	return ch
}

func (s *Server) block(r *http.Request, name string) bool {
	s.mu.Lock()
	release := s.chanFor(s.releases, name)
	closeOnce(s.chanFor(s.blocked, name))
	s.mu.Unlock()
	select {
	case <-release:
		return true
	case <-r.Context().Done():
		s.mu.Lock()
		closeOnce(s.chanFor(s.canceled, name))
		s.mu.Unlock()
	case <-s.closing:
	}
	return false
}

func (s *Server) selectStep(req Request) int {
	s.selMu.Lock()
	defer s.selMu.Unlock()
	s.mu.Lock()
	var candidates []int
	for i := range s.steps {
		if !s.consumed[i] {
			candidates = append(candidates, i)
		}
	}
	s.mu.Unlock()
	for _, i := range candidates {
		if !s.steps[i].Match(req) {
			continue
		}
		s.mu.Lock()
		ok := !s.consumed[i]
		s.consumed[i] = !s.steps[i].Repeat
		s.mu.Unlock()
		if ok {
			return i
		}
	}
	return -1
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if suffix := s.wire().pathSuffix; suffix != "" && !strings.HasSuffix(r.URL.Path, suffix) {
		s.mu.Lock()
		s.undecoded = append(s.undecoded, fmt.Sprintf("request path %q does not end in %q", r.URL.Path, suffix))
		s.mu.Unlock()
		s.wire().writeError(w, http.StatusNotFound, "harnesstest: unknown path")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.wire().writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := s.wire().decode(body, r.Header)
	if err != nil {
		s.mu.Lock()
		s.undecoded = append(s.undecoded, fmt.Sprintf("%v (body prefix %q)", err, body[:min(len(body), 80)]))
		s.mu.Unlock()
		s.wire().writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	n := len(s.requests)
	close(s.arrived)
	s.arrived = make(chan struct{})
	s.mu.Unlock()

	matched := s.selectStep(req)

	s.mu.Lock()
	var step Step
	if matched < 0 {
		s.unmatched = append(s.unmatched, req)
	} else {
		step = s.steps[matched]
	}
	s.mu.Unlock()

	switch {
	case matched < 0:
		s.wire().writeError(w, http.StatusInternalServerError, "harnesstest: no step matched")
	case step.Reply.HTTPStatus != 0:
		s.wire().replyError(w, step.Reply)
	default:
		s.wire().stream(s, w, r, n, step.Name, step.Reply)
	}
}
