package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
)

// fakeClientAPI is a plugin.ClientAPI whose responses are distinctive
// enough that a test can tell "delegated to this implementation" apart
// from "got lateClientAPI's own not-ready error" or a zero value.
type fakeClientAPI struct{}

func (f *fakeClientAPI) SessionMessages(_ context.Context, req *plugin.SessionMessagesRequest) (*plugin.SessionMessagesResponse, error) {
	return &plugin.SessionMessagesResponse{Messages: []message.Message{
		{Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "fake:" + req.SessionID}}},
	}}, nil
}

func (f *fakeClientAPI) MCPCall(_ context.Context, req *plugin.MCPCallRequest) (*plugin.MCPCallResult, error) {
	return &plugin.MCPCallResult{Content: message.Parts{&message.Text{Text: "fake:" + req.Tool}}}, nil
}

func (f *fakeClientAPI) Generate(_ context.Context, req *plugin.GenerateRequest) (*plugin.GenerateResponse, error) {
	return &plugin.GenerateResponse{Message: message.Message{
		Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "fake:" + req.Model}},
	}}, nil
}

// TestLateClientAPIPreBindNotReady covers all three methods before Bind is
// ever called: each must return lateClientAPI's own not-ready error, not a
// nil-pointer panic and not silently zero-valued success.
func TestLateClientAPIPreBindNotReady(t *testing.T) {
	l := newLateClientAPI()

	if _, err := l.SessionMessages(context.Background(), &plugin.SessionMessagesRequest{SessionID: "s1"}); err == nil {
		t.Error("SessionMessages before Bind: want an error, got nil")
	} else if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("SessionMessages before Bind: err = %q, want it to mention not ready", err)
	}

	if _, err := l.MCPCall(context.Background(), &plugin.MCPCallRequest{Server: "gw", Tool: "noop"}); err == nil {
		t.Error("MCPCall before Bind: want an error, got nil")
	} else if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("MCPCall before Bind: err = %q, want it to mention not ready", err)
	}

	if _, err := l.Generate(context.Background(), &plugin.GenerateRequest{Model: "fast"}); err == nil {
		t.Error("Generate before Bind: want an error, got nil")
	} else if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("Generate before Bind: err = %q, want it to mention not ready", err)
	}
}

// TestLateClientAPIConcurrentBindDelegatesCorrectly drives all three
// methods from many goroutines concurrently with a single Bind call racing
// them, under -race. Every observed result must be one of exactly two
// shapes: lateClientAPI's own not-ready error (Bind hadn't landed yet for
// that call) or the fake implementation's distinctive response (Bind had
// landed) — never a nil-pointer panic, a spurious error, or a zero-valued
// success that would mean the call silently skipped the fake entirely.
func TestLateClientAPIConcurrentBindDelegatesCorrectly(t *testing.T) {
	l := newLateClientAPI()
	fake := &fakeClientAPI{}

	const callersPerMethod = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	var notReady, delegated int

	record := func(ok bool) {
		mu.Lock()
		if ok {
			delegated++
		} else {
			notReady++
		}
		mu.Unlock()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.Bind(fake)
	}()

	for i := 0; i < callersPerMethod; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			resp, err := l.SessionMessages(context.Background(), &plugin.SessionMessagesRequest{SessionID: "s1"})
			switch {
			case err == nil:
				if resp == nil || len(resp.Messages) != 1 || resp.Messages[0].Parts.Text() != "fake:s1" {
					t.Errorf("SessionMessages call %d: unexpected success payload %+v", i, resp)
				}
				record(true)
			case strings.Contains(err.Error(), "not ready"):
				record(false)
			default:
				t.Errorf("SessionMessages call %d: unexpected error %v", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			resp, err := l.MCPCall(context.Background(), &plugin.MCPCallRequest{Server: "gw", Tool: "noop"})
			switch {
			case err == nil:
				if resp == nil || resp.Content.Text() != "fake:noop" {
					t.Errorf("MCPCall call %d: unexpected success payload %+v", i, resp)
				}
				record(true)
			case strings.Contains(err.Error(), "not ready"):
				record(false)
			default:
				t.Errorf("MCPCall call %d: unexpected error %v", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			resp, err := l.Generate(context.Background(), &plugin.GenerateRequest{Model: "fast"})
			switch {
			case err == nil:
				if resp == nil || resp.Message.Parts.Text() != "fake:fast" {
					t.Errorf("Generate call %d: unexpected success payload %+v", i, resp)
				}
				record(true)
			case strings.Contains(err.Error(), "not ready"):
				record(false)
			default:
				t.Errorf("Generate call %d: unexpected error %v", i, err)
			}
		}(i)
	}

	wg.Wait()

	if delegated == 0 {
		t.Error("no call ever observed the bound implementation — Bind never took effect")
	}
	if notReady+delegated != 3*callersPerMethod {
		t.Errorf("accounted for %d calls, want %d", notReady+delegated, 3*callersPerMethod)
	}

	// After Wait, Bind has definitely landed: every call from here on must
	// delegate.
	if _, err := l.SessionMessages(context.Background(), &plugin.SessionMessagesRequest{SessionID: "s2"}); err != nil {
		t.Errorf("SessionMessages after Bind settled: %v", err)
	}
}
