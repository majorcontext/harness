// Package e2e provides a REAL, non-mocked backend for verifying tools/hub's
// incremental-rendering behavior end-to-end: a real server.Server (the same
// wiring as `harness serve`) fed by a small scripted provider (no API key
// needed), and a real hub.NewHandler serving the ACTUAL embedded
// tools/hub/index.html (the same wiring as `harness hub`). Nothing here
// mocks the HTTP/SSE transport — the page's own fetch/EventSource-style
// reader loop talks to a real net/http server over a real loopback socket.
//
// This exists specifically to answer "does the incremental-rendering fix
// actually work against a real harness box, or only against hand-rolled
// mocks in a JS unit test?" — see e2e_test.go, which drives the real page
// (via Node + jsdom, since this repo's UI has no other DOM available) with
// Node's own, unmocked fetch against the servers Start returns.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/server"
	"github.com/majorcontext/harness/tools/hub"
)

// RunToken is the fixed run token the stub box authenticates with.
const RunToken = "verify-token"

// ProviderName is the scripted provider's family, used as the model's
// provider segment (e.g. "verify/m1").
const ProviderName = "verify"

// scriptedProvider serves one canned turn per call: a reasoning delta, a
// text delta, then a done event carrying the fully assembled message —
// enough to exercise the hub's reasoning-block + streaming-draft +
// durable-message rendering without needing a real model API key. Turns are
// numbered from 1 and are distinguishable in the rendered text ("reply
// number N"), so a test can tell turns apart without needing exact counts.
type scriptedProvider struct {
	mu   sync.Mutex
	call int
}

func (p *scriptedProvider) Name() string { return ProviderName }

func (p *scriptedProvider) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	n := p.call
	p.call++
	p.mu.Unlock()
	text := fmt.Sprintf("reply number %d", n+1)
	reasoning := fmt.Sprintf("thinking about turn %d", n+1)
	msg := &message.Message{
		ID:   message.ProviderCallID("m", text, 16),
		Role: message.RoleAssistant,
		Parts: message.Parts{
			&message.Reasoning{Text: reasoning},
			&message.Text{Text: text},
		},
	}
	events := []provider.Event{
		{Type: provider.EventReasoningDelta, Text: reasoning},
		{Type: provider.EventTextDelta, Text: text},
		{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn},
	}
	return &scriptedStream{events: events}, nil
}

type scriptedStream struct {
	events []provider.Event
	i      int
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.i >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

func (s *scriptedStream) Close() error { return nil }

// Stub is a running (box server, hub server) pair plus its teardown.
type Stub struct {
	BoxBase string // e.g. "http://127.0.0.1:54321" — a real harness serve-equivalent
	HubBase string // e.g. "http://127.0.0.1:54322" — a real harness hub-equivalent, serving the real index.html
	Token   string

	boxLn      net.Listener
	hubLn      net.Listener
	srv        *server.Server
	sessionDir string
}

// Start builds and starts a real box server (server.New, scripted provider,
// CORS wide open) and a real hub server (hub.NewHandler, the actual embedded
// index.html) on loopback, each on an OS-assigned port. Close tears both
// down. SessionDir is a fresh temp directory (real on-disk journal, same as
// production), removed by Close.
func Start() (*Stub, error) {
	dir, err := os.MkdirTemp("", "hub-e2e-sessions")
	if err != nil {
		return nil, err
	}
	reg := provider.Registry{ProviderName: &scriptedProvider{}}
	model := message.ModelRef{Provider: ProviderName, Model: "m1"}

	var srv *server.Server
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:  reg,
			Model:      m,
			WorkDir:    dir,
			SessionDir: dir,
			OnEvent:    func(ev engine.Event) { srv.Publish(ev) },
		}
	}
	srv, err = server.New(server.Options{
		SessionDir: dir,
		RunToken:   RunToken,
		Version:    "hub-e2e",
		CORSOrigin: "*",
		NewSession: func(m message.ModelRef, workDir string, parentSession string) (*engine.Session, error) {
			if m.Provider == "" {
				m = model
			}
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parentSession
			return engine.NewSession(cfg), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(mkCfg(model), id)
		},
	})
	if err != nil {
		os.RemoveAll(dir) //nolint:errcheck
		return nil, err
	}

	boxLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		srv.Close() //nolint:errcheck
		os.RemoveAll(dir)
		return nil, err
	}
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		boxLn.Close() //nolint:errcheck
		srv.Close()   //nolint:errcheck
		os.RemoveAll(dir)
		return nil, err
	}

	go http.Serve(boxLn, srv)                           //nolint:errcheck
	go http.Serve(hubLn, hub.NewHandler(hub.Options{})) //nolint:errcheck

	return &Stub{
		BoxBase:    "http://" + boxLn.Addr().String(),
		HubBase:    "http://" + hubLn.Addr().String(),
		Token:      RunToken,
		boxLn:      boxLn,
		hubLn:      hubLn,
		srv:        srv,
		sessionDir: dir,
	}, nil
}

// Close tears down both listeners, closes the server, and removes the
// temporary session directory. Safe to defer immediately after Start.
func (s *Stub) Close() {
	if s == nil {
		return
	}
	if s.boxLn != nil {
		s.boxLn.Close() //nolint:errcheck
	}
	if s.hubLn != nil {
		s.hubLn.Close() //nolint:errcheck
	}
	if s.srv != nil {
		s.srv.Close() //nolint:errcheck
	}
	if s.sessionDir != "" {
		os.RemoveAll(s.sessionDir) //nolint:errcheck
	}
}
