package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/openai"
)

// codexWSStub answers each response.create frame with one scripted set of
// frames and records the body it was sent.
type codexWSStub struct {
	*httptest.Server
	mu      sync.Mutex
	scripts [][]string
	bodies  []map[string]any
	onFrame func(n int)
}

func newCodexWSStub(t *testing.T, scripts [][]string, onFrame func(n int)) *codexWSStub {
	t.Helper()
	st := &codexWSStub{scripts: scripts, onFrame: onFrame}
	st.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			_, frame, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var body map[string]any
			_ = json.Unmarshal(frame, &body)
			// A generate:false prewarm is not a model call.
			if gen, ok := body["generate"].(bool); ok && !gen {
				for _, f := range prewarmAckFrames() {
					if conn.Write(context.Background(), websocket.MessageText, []byte(f)) != nil {
						return
					}
				}
				continue
			}
			st.mu.Lock()
			st.bodies = append(st.bodies, body)
			n := len(st.bodies)
			var script []string
			if n-1 < len(st.scripts) {
				script = st.scripts[n-1]
			}
			st.mu.Unlock()
			if st.onFrame != nil {
				st.onFrame(n)
			}
			for _, f := range script {
				if conn.Write(context.Background(), websocket.MessageText, []byte(f)) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(st.Close)
	return st
}

func (st *codexWSStub) body(i int) map[string]any {
	st.mu.Lock()
	defer st.mu.Unlock()
	if i >= len(st.bodies) {
		return nil
	}
	return st.bodies[i]
}

func prewarmAckFrames() []string {
	return []string{
		`{"type":"response.created","response":{"id":"resp_prewarm"}}`,
		`{"type":"response.completed","response":{"id":"resp_prewarm"}}`,
	}
}

func toolCallFrames(respID, callID, name, args string) []string {
	return []string{
		`{"type":"response.created","response":{"id":"` + respID + `"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"` + callID + `","name":"` + name + `","arguments":"` + args + `"}}`,
		`{"type":"response.completed","response":{"id":"` + respID + `"}}`,
	}
}

func finalTextFrames(respID, text string) []string {
	return []string{
		`{"type":"response.created","response":{"id":"` + respID + `"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"` + text + `"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`,
		`{"type":"response.completed","response":{"id":"` + respID + `"}}`,
	}
}

// mutableProcessRegistry stands in for the box-scoped registry, whose state
// a test changes between two model calls.
type mutableProcessRegistry struct {
	mu   sync.Mutex
	info process.Info
}

func (r *mutableProcessRegistry) set(info process.Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.info = info
}

func (r *mutableProcessRegistry) List() []process.Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []process.Info{r.info}
}

func (r *mutableProcessRegistry) Start(context.Context, string) (process.Status, error) {
	return process.Status{}, nil
}
func (r *mutableProcessRegistry) Stop(context.Context, string) (process.Status, error) {
	return process.Status{}, nil
}
func (r *mutableProcessRegistry) Restart(context.Context, string) (process.Status, error) {
	return process.Status{}, nil
}
func (r *mutableProcessRegistry) Status(string) (process.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.info.Status, nil
}
func (r *mutableProcessRegistry) Logs(string, int) (string, process.Status, error) {
	return "", process.Status{}, nil
}
func (r *mutableProcessRegistry) Declare(string, process.Def) error { return nil }
func (r *mutableProcessRegistry) Undeclare(string) error            { return nil }
func (r *mutableProcessRegistry) EverStarted() bool                 { return true }

func runningInfo(at time.Time) process.Info {
	return process.Info{Name: "app-dev", Status: process.Status{
		Name: "app-dev", State: process.StateReady, StartedAt: at, Ready: true,
		Log: "/work/.harness/proc/app-dev.log",
	}}
}

func stoppedInfo(at time.Time) process.Info {
	return process.Info{Name: "app-dev", Status: process.Status{
		Name: "app-dev", State: process.StateStopped, FinishedAt: at,
		Log: "/work/.harness/proc/app-dev.log",
	}}
}

// codexWSSession drives the real native Codex adapter over its WebSocket
// transport, pointed at stub.
func codexWSSession(t *testing.T, stub *codexWSStub, reg ProcessRegistry, onMetrics func(TurnMetrics)) *Session {
	t.Helper()
	client := &openai.Client{
		APIKey:                "test",
		BaseURL:               stub.URL,
		Family:                openai.CodexFamily,
		UseWebSocketTransport: true,
	}
	return NewSession(Config{
		Providers:     provider.Registry{openai.CodexFamily: client},
		Model:         message.ModelRef{Provider: openai.CodexFamily, Model: "gpt-5"},
		Processes:     reg,
		WorkDir:       t.TempDir(),
		Tools:         []Tool{echoTool()},
		OnTurnMetrics: onMetrics,
	})
}

func echoTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "noop",
			Description: "does nothing",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		Run: func(context.Context, *Session, json.RawMessage) (message.Parts, error) {
			return message.Parts{&message.Text{Text: "ok"}}, nil
		},
	}
}

func refusedItem(m TurnMetrics) string {
	if m.ChainRefusalItem == nil {
		return "none"
	}
	return strconv.Itoa(*m.ChainRefusalItem)
}

// Input: two model calls of one tool loop, with a process state change
// between them. Wrong output: the second call rewrites the newest user input
// item, so the Codex pool reports prefix_changed and re-sends every item
// uncached instead of projecting a suffix.
func TestAmbientProcessTransitionKeepsCodexPrefixStable(t *testing.T) {
	reg := &mutableProcessRegistry{}
	reg.set(runningInfo(time.Date(2026, 9, 10, 0, 27, 0, 0, time.UTC)))

	stub := newCodexWSStub(t, [][]string{
		toolCallFrames("resp_1", "call_1", "noop", "{}"),
		finalTextFrames("resp_2", "done"),
	}, func(n int) {
		if n == 1 {
			reg.set(stoppedInfo(time.Date(2026, 9, 10, 0, 29, 0, 0, time.UTC)))
		}
	})

	var mu sync.Mutex
	var metrics []TurnMetrics
	s := codexWSSession(t, stub, reg, func(m TurnMetrics) {
		mu.Lock()
		metrics = append(metrics, m)
		mu.Unlock()
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(metrics) != 2 {
		t.Fatalf("completed model calls = %d, want 2", len(metrics))
	}
	second := metrics[1]
	if second.ChainRefusal != provider.ChainRefusalNone {
		t.Errorf("second call refused to chain: %q at item %s — an ambient process transition rewrote an item already in the prefix",
			second.ChainRefusal, refusedItem(second))
	}
	if second.RequestMode != provider.RequestModeIncremental {
		t.Errorf("second call request_mode = %q, want %q (suffix projection)", second.RequestMode, provider.RequestModeIncremental)
	}
	if second.SentInputItems >= second.CompleteInputItems {
		t.Errorf("second call sent %d of %d input items, want a strict suffix", second.SentInputItems, second.CompleteInputItems)
	}
}

// Config.Processes is an interface, so configSnapshot hands a child the same
// registry its parent mutates.
//
// Input: a child making no process call, whose parent changes process state
// mid tool loop. Wrong output: the child reports prefix_changed at item 0 and
// re-sends every item uncached.
func TestChildInheritsSharedProcessRegistryWithoutBreakingItsChain(t *testing.T) {
	reg := &mutableProcessRegistry{}
	reg.set(runningInfo(time.Date(2026, 9, 10, 0, 27, 0, 0, time.UTC)))

	stub := newCodexWSStub(t, [][]string{
		toolCallFrames("resp_c1", "call_1", "noop", "{}"),
		finalTextFrames("resp_c2", "done"),
	}, func(n int) {
		if n == 1 {
			reg.set(stoppedInfo(time.Date(2026, 9, 10, 0, 29, 0, 0, time.UTC)))
		}
	})

	var mu sync.Mutex
	var metrics []TurnMetrics
	parent := codexWSSession(t, stub, reg, func(m TurnMetrics) {
		mu.Lock()
		metrics = append(metrics, m)
		mu.Unlock()
	})

	childCfg := parent.configSnapshot()
	if childCfg.Processes != ProcessRegistry(reg) {
		t.Fatal("child config does not share the parent's process registry; this test no longer reproduces the reported cause")
	}
	child := NewSession(childCfg)

	if _, err := child.Prompt(context.Background(), "child work"); err != nil {
		t.Fatalf("child Prompt: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(metrics) != 2 {
		t.Fatalf("completed child model calls = %d, want 2", len(metrics))
	}
	second := metrics[1]
	if second.ChainRefusal != provider.ChainRefusalNone {
		t.Errorf("child refused to chain (%q at item %s) because its PARENT changed shared process state",
			second.ChainRefusal, refusedItem(second))
	}
	if second.RequestMode != provider.RequestModeIncremental || second.SentInputItems >= second.CompleteInputItems {
		t.Errorf("child second call = %q sending %d of %d items, want a strict suffix",
			second.RequestMode, second.SentInputItems, second.CompleteInputItems)
	}
}
