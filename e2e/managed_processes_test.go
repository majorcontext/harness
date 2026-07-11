package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestManagedProcessesEndToEnd is the CI-enforced proof of the whole
// managed-processes feature's actual PURPOSE, driven through a real
// `harness serve` subprocess, a real session, and a real (if short-lived)
// child process — not just unit-level calls into engine/process types:
//
//  1. An agent asks, in ordinary English, for a dev server to be started.
//     Nothing in the prompt names a tool, a log path, or "process" at all —
//     the model discovers and calls the `process` tool itself, and Start
//     really spawns `sh` and really blocks on the configured ready_regex.
//     This is "agents start dev servers without ceremony."
//  2. The VERY NEXT model request in the same tool loop — before the
//     caller has said one more word — already carries the process's live
//     state in an ambient status block riding the newest user message. The
//     prompt text driving that request is unchanged ("start the dev
//     server"): the model was never told a second time.
//  3. A wholly separate, later prompt ("what should I do next?") that
//     never mentions processes at all STILL carries the ambient block,
//     proving this is a standing fact about every request, not a one-shot
//     echo of the tool result. This is "the model always knows what is
//     running and where the logs are WITHOUT being told in the prompt."
//  4. The real log file the real child process wrote to is inspected
//     directly on disk, and GET /process / POST /process/dev/stop are
//     exercised against the same live process the model started.
func TestManagedProcessesEndToEnd(t *testing.T) {
	skipShort(t)

	fake := newProcessToolAnthropic()
	fakeSrv := httptest.NewServer(fake)
	t.Cleanup(fakeSrv.Close)

	sessDir := t.TempDir()
	workDir := t.TempDir()
	cfgPath := writeProcessConfig(t, fakeSrv.URL)

	p := startServeIn(t, sessDir, cfgPath, workDir)
	id := p.createSession()

	// --- 1. ceremony-free start, driven entirely by the model's own tool call ---
	p.prompt(id, "start the dev server")
	// user, assistant(tool_call), tool(result), assistant(final) = 4 durable messages.
	msgs := p.waitMessages(id, 4)
	if msgs[0].Role != "user" || textOf(msgs[0]) != "start the dev server" {
		t.Fatalf("first message = %+v, want the verbatim user prompt", msgs[0])
	}

	bodies := fake.snapshot()
	if len(bodies) < 2 {
		t.Fatalf("fake provider saw %d requests, want at least 2 (tool-call turn + follow-up)", len(bodies))
	}

	// The FIRST request (before the tool ever ran) must carry no ambient
	// status at all: nothing has ever been started yet.
	if hasProcessesBlock(t, bodies[0]) {
		t.Errorf("ambient status block present on the very first request, before anything was ever started:\n%s", bodies[0])
	}

	// --- 2. the tool-loop's own follow-up request already knows, unprompted ---
	second := bodies[1]
	if !hasProcessesBlock(t, second) {
		t.Fatalf("ambient status block missing from the tool-loop follow-up request (turn 2):\n%s", second)
	}
	if !strings.Contains(string(second), "dev") {
		t.Errorf("ambient status block does not name the process 'dev':\n%s", second)
	}

	// --- 4a. inspect the /process HTTP surface for the same live process ---
	resp, data := p.do(http.MethodGet, "/process", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /process: status %d body %s", resp.StatusCode, data)
	}
	var infos []struct {
		Name   string `json:"name"`
		Status struct {
			State string `json:"state"`
			Log   string `json:"log"`
			Ready bool   `json:"ready"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &infos); err != nil {
		t.Fatalf("decode /process: %v (%s)", err, data)
	}
	if len(infos) != 1 || infos[0].Name != "dev" {
		t.Fatalf("GET /process = %+v, want exactly one process named dev", infos)
	}
	if infos[0].Status.State != "ready" || !infos[0].Status.Ready {
		t.Fatalf("GET /process status = %+v, want ready", infos[0].Status)
	}
	wantLog := filepath.Join(workDir, ".harness", "proc", "dev.log")
	if infos[0].Status.Log != wantLog {
		t.Errorf("log path = %q, want %q", infos[0].Status.Log, wantLog)
	}

	// --- 4b. the real child process really wrote the log file on disk ---
	logBytes, err := os.ReadFile(wantLog)
	if err != nil {
		t.Fatalf("reading real log file: %v", err)
	}
	if !strings.Contains(string(logBytes), "Ready in") {
		t.Errorf("log file content = %q, want the real ready line the child process printed", logBytes)
	}

	// --- 3. a later, unrelated prompt STILL knows, without being told again ---
	p.prompt(id, "what should I do next?")
	msgs = p.waitMessages(id, 6)
	if textOf(msgs[len(msgs)-2]) != "what should I do next?" {
		t.Fatalf("message before the final reply = %+v, want the verbatim later prompt", msgs[len(msgs)-2])
	}

	bodies = fake.snapshot()
	last := bodies[len(bodies)-1]
	if !hasProcessesBlock(t, last) {
		t.Fatalf("ambient status block missing from a later, unrelated prompt — the model was not told what is running without being told:\n%s", last)
	}
	if !strings.Contains(string(last), "ready") {
		t.Errorf("later request's ambient block does not report the live 'ready' state:\n%s", last)
	}

	// --- clean up the real process through the same HTTP surface ---
	resp, data = p.do(http.MethodPost, "/process/dev/stop", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /process/dev/stop: status %d body %s", resp.StatusCode, data)
	}
}

// writeProcessConfig writes a harness config pointing the anthropic
// provider at baseURL and declaring one managed process ("dev": a real
// `sh` script that prints a ready line and then blocks, mirroring a real
// `pnpm dev`), returning its path.
func writeProcessConfig(t *testing.T, baseURL string) string {
	t.Helper()
	cfg := map[string]any{
		"model": "anthropic/claude-fable-5",
		"providers": map[string]any{
			"anthropic": map[string]any{
				"api_key_env": "ANTHROPIC_API_KEY",
				"base_url":    baseURL,
			},
		},
		"processes": map[string]any{
			"dev": map[string]any{
				"command":         []string{"sh", "-c", `echo "Ready in 12ms"; sleep 100`},
				"ready_regex":     "Ready in .*ms",
				"ready_timeout_s": 10,
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// hasProcessesBlock reports whether raw (a captured Anthropic Messages API
// request body) contains a "[processes: ...]" ambient status block in any
// user-role text content — the wire-level proof that the injection
// actually reached the request the model receives, not just an in-process
// canonical message.
func hasProcessesBlock(t *testing.T, raw []byte) bool {
	t.Helper()
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode captured request body: %v (%s)", err, raw)
	}
	for _, m := range body.Messages {
		if m.Role != "user" {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "text" && strings.Contains(c.Text, "[processes:") {
				return true
			}
		}
	}
	return false
}

// processToolAnthropic is a fake Anthropic Messages API that records every
// request's raw body and, on its first call, emits a tool_use block
// calling the `process` tool (action start, name dev) — so the served
// session's model turn is the one actually driving Start, exactly as a
// real agent would. Every later call answers a plain end_turn text reply.
type processToolAnthropic struct {
	mu     sync.Mutex
	bodies [][]byte
	count  int
}

func newProcessToolAnthropic() *processToolAnthropic {
	return &processToolAnthropic{}
}

func (f *processToolAnthropic) snapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
}

func (f *processToolAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.bodies = append(f.bodies, raw)
	n := f.count + 1
	f.count = n
	f.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	if n == 1 {
		io.WriteString(w, toolCallTurn(
			fmt.Sprintf("msg_%d", n),
			"toolu_start_dev",
			"process",
			`{"action":"start","name":"dev"}`,
		))
		flusher.Flush()
		return
	}

	io.WriteString(w, completeTurn(fmt.Sprintf("msg_%d", n), fmt.Sprintf("ok (turn %d)", n)))
	flusher.Flush()
}

// toolCallTurn is a complete SSE turn ending in a single tool_use block
// (shape matches provider/anthropic/stream_test.go's tool_use fixture).
func toolCallTurn(msgID, toolUseID, toolName, inputJSON string) string {
	return strings.Join([]string{
		sse("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"usage":{"input_tokens":5}}}`, msgID)),
		sse("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, toolUseID, toolName)),
		sse("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, mustQuoteJSONString(inputJSON))),
		sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}`),
		sse("message_stop", `{"type":"message_stop"}`),
	}, "")
}

// mustQuoteJSONString renders s (itself already a JSON document, e.g.
// `{"action":"start"}`) as a JSON string literal, so it can be embedded as
// an SSE event's "partial_json" field value.
func mustQuoteJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
