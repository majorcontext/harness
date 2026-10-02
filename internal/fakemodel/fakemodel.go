// Package fakemodel is a scripted Anthropic Messages server for tests.
package fakemodel

import (
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
	Block      bool       // stream Text's first delta, then wait for Release or client cancel
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
	releases  map[string]chan struct{}
	onRequest func(n int)
}

func New(t testing.TB, steps ...Step) *Server {
	t.Helper()
	s := &Server{
		closing:  make(chan struct{}),
		steps:    steps,
		consumed: make([]bool, len(steps)),
		releases: map[string]chan struct{}{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() {
		close(s.closing)
		s.srv.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
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
	ch := s.releaseChan(stepName)
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

func (s *Server) releaseChan(name string) chan struct{} {
	ch, ok := s.releases[name]
	if !ok {
		ch = make(chan struct{})
		s.releases[name] = ch
	}
	return ch
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := decodeRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	n := len(s.requests)
	matched := -1
	for i, st := range s.steps {
		if s.consumed[i] || st.Match == nil || !st.Match(req) {
			continue
		}
		matched = i
		s.consumed[i] = !st.Repeat
		break
	}
	var step Step
	var release chan struct{}
	if matched < 0 {
		s.unmatched = append(s.unmatched, req)
	} else {
		step = s.steps[matched]
		release = s.releaseChan(step.Name)
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
		s.stream(w, r, n, step.Reply, release)
	}
}
