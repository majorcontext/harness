// Package fakemodel is a scripted Anthropic Messages server for tests.
package fakemodel

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type Step struct {
	Name   string
	Match  Matcher
	Reply  Reply
	Repeat bool // stays available after it matches
}

type Reply struct {
	Text       string
	ToolCalls  []ToolCall // emitted as tool_use blocks after Text
	StopReason string     // default "end_turn", or "tool_use" when ToolCalls is set
	Usage      Usage      // default {Input: 5, Output: 3}
	HTTPStatus int        // non-zero: reply with this status and an Anthropic error body
	Block      bool       // after the first content delta, wait for Release or client cancel; a Repeat step blocks only until its first Release
}

type ToolCall struct {
	ID, Name string
	Input    map[string]any
}

type Usage struct{ Input, Output int }

type Request struct {
	System   string
	Messages []Message
	Tools    []string // sorted names
}

type Message struct {
	Role  string
	Parts []Part
}

type Part struct {
	Kind      string // "text" | "tool_use" | "tool_result"
	Text      string
	ToolName  string
	ToolInput map[string]any
	ToolUseID string
	IsError   bool
}

// LastUserText is the text of the last user message that has a text part.
func (r Request) LastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != "user" {
			continue
		}
		var texts []string
		for _, p := range r.Messages[i].Parts {
			if p.Kind == "text" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

type Server struct {
	srv     *httptest.Server
	closing chan struct{}

	mu        sync.Mutex
	steps     []Step
	consumed  []bool
	requests  []Request
	unmatched []Request
	undecoded []string
	releases  map[string]chan struct{}
	blocked   map[string]chan struct{}
	onRequest func(n int)
}

func New(t testing.TB, steps ...Step) *Server {
	t.Helper()
	steps = append([]Step(nil), steps...)
	for i := range steps {
		if steps[i].Match == nil {
			steps[i].Match = Any()
		}
	}
	s := &Server{
		closing:  make(chan struct{}),
		steps:    steps,
		consumed: make([]bool, len(steps)),
		releases: map[string]chan struct{}{},
		blocked:  map[string]chan struct{}{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() {
		close(s.closing)
		s.srv.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, msg := range s.undecoded {
			t.Errorf("fakemodel: undecodable request body: %s", msg)
		}
		for _, r := range s.unmatched {
			sys := r.System
			if len(sys) > 80 {
				sys = sys[:80]
			}
			t.Errorf("fakemodel: no step matched request: last user text %q, system prefix %q", r.LastUserText(), sys)
		}
	})
	return s
}

func (s *Server) URL() string { return s.srv.URL }

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) Release(stepName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	closeOnce(s.chanFor(s.releases, stepName))
}

// Blocked is closed once a request for the named Block step is waiting.
func (s *Server) Blocked(stepName string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chanFor(s.blocked, stepName)
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (s *Server) OnRequest(f func(n int)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRequest = f
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
	case <-s.closing:
	}
	return false
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := decodeRequest(body)
	if err != nil {
		s.mu.Lock()
		s.undecoded = append(s.undecoded, fmt.Sprintf("%v (body prefix %q)", err, body[:min(len(body), 80)]))
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	n := len(s.requests)
	matched := -1
	for i, st := range s.steps {
		if s.consumed[i] || !st.Match(req) {
			continue
		}
		matched = i
		s.consumed[i] = !st.Repeat
		break
	}
	var step Step
	if matched < 0 {
		s.unmatched = append(s.unmatched, req)
	} else {
		step = s.steps[matched]
	}
	onRequest := s.onRequest
	s.mu.Unlock()

	if onRequest != nil {
		onRequest(n)
	}
	switch {
	case matched < 0:
		writeError(w, http.StatusInternalServerError, "fakemodel: no step matched")
	case step.Reply.HTTPStatus != 0:
		writeError(w, step.Reply.HTTPStatus, "fakemodel: scripted error")
	default:
		s.stream(w, r, n, step.Name, step.Reply)
	}
}
