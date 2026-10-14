package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// soakEnv enables the soak test. It needs the tools of the browser job.
const soakEnv = "HARNESS_E2E_SOAK"

const (
	soakTurns  = 300
	soakDirs   = 60
	soakPerDir = 50
	// soakWindow makes a reading of soakSpikeInput tokens pass the compaction threshold.
	soakWindow     = 1000
	soakSpikeInput = 900
	// The bounds only catch a gross regression; the measured numbers are the result.
	soakReplayBound = 10 * time.Second
	soakMemoryBound = 512.0
)

var soakSpikes = map[int]bool{100: true, 200: true}

// soakProbe is appended to wasm_exec.js for the soak row. It answers a
// {probe} message of the page with the linear memory of the module.
const soakProbe = `
;(() => {
  const instantiate = WebAssembly.instantiate;
  let mem;
  WebAssembly.instantiate = async function (...args) {
    const r = await instantiate.apply(this, args);
    mem = (r.instance ?? r).exports.mem;
    return r;
  };
  self.addEventListener("message", (e) => {
    if (e.data && e.data.probe) {
      self.postMessage({ probe: { id: e.data.probe, wasmBytes: mem ? mem.buffer.byteLength : 0 } });
    }
  });
})();
`

// serveWithProbe writes wasm_exec.js and the memory probe.
func serveWithProbe(w http.ResponseWriter, execJS string) {
	src, err := os.ReadFile(execJS)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(src)
	_, _ = io.WriteString(w, soakProbe)
}

var soakTurnNumber = regexp.MustCompile(`soak turn (\d+):`)

func soakLastIsResult(r harnesstest.Request) bool {
	if len(r.Messages) == 0 {
		return false
	}
	for _, p := range r.Messages[len(r.Messages)-1].Parts {
		if p.Kind == "tool_result" {
			return true
		}
	}
	return false
}

// soakSteps scripts a long session: each turn calls one tool, then answers.
// The answer of a spike turn reports input tokens above the compaction threshold.
func soakSteps() []harnesstest.Step {
	turn := func(r harnesstest.Request) (int, bool) {
		m := soakTurnNumber.FindStringSubmatch(r.LastUserText())
		if m == nil {
			return 0, false
		}
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	answer := strings.Repeat("The scripted answer of the soak session. ", 10)
	return []harnesstest.Step{
		{Name: "summary", Repeat: true, Match: harnesstest.SystemContains("You are summarizing a prefix"),
			Reply: harnesstest.Reply{Text: strings.Repeat("Summary of the earlier turns of the soak session. ", 20)}},
		{Name: "grep call", Repeat: true, Match: func(r harnesstest.Request) bool {
			return strings.HasPrefix(r.LastUserText(), "soak grep") && !soakLastIsResult(r)
		}, Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "grep1", Name: "grep", Input: map[string]any{"pattern": "needle_[0-9]+_0", "path": "/work/tree"}}}}},
		{Name: "grep answer", Repeat: true, Match: func(r harnesstest.Request) bool {
			return strings.HasPrefix(r.LastUserText(), "soak grep") && soakLastIsResult(r)
		}, Reply: harnesstest.Reply{Text: "grep done"}},
		{Name: "call", Repeat: true, Match: func(r harnesstest.Request) bool {
			_, ok := turn(r)
			return ok && !soakLastIsResult(r)
		}, Reply: harnesstest.Reply{Calls: func(r harnesstest.Request) []harnesstest.ToolCall {
			n, _ := turn(r)
			id := "soak" + strconv.Itoa(n)
			if n%3 == 0 {
				return []harnesstest.ToolCall{{ID: id, Name: "write_file", Input: map[string]any{
					"path": fmt.Sprintf("/work/soak/t%d.txt", n), "content": strings.Repeat("written by a soak turn\n", 20)}}}
			}
			return []harnesstest.ToolCall{{ID: id, Name: "js_echo", Input: map[string]any{"text": strings.Repeat("echo ", 100)}}}
		}}},
		{Name: "spike answer", Repeat: true, Match: func(r harnesstest.Request) bool {
			n, ok := turn(r)
			return ok && soakSpikes[n] && soakLastIsResult(r)
		}, Reply: harnesstest.Reply{Text: answer, Usage: harnesstest.Usage{Input: soakSpikeInput, Output: 100}}},
		{Name: "answer", Repeat: true, Match: func(r harnesstest.Request) bool {
			_, ok := turn(r)
			return ok && soakLastIsResult(r)
		}, Reply: harnesstest.Reply{Text: answer}},
	}
}

// soakPhase is the memory and the longest main-thread task at the end of a phase.
type soakPhase struct {
	PageHeapMB    float64 `json:"pageHeapMB"`
	WasmMB        float64 `json:"wasmMB"`
	LongTaskMaxMs float64 `json:"longTaskMaxMs"`
	LagMaxMs      float64 `json:"lagMaxMs"`
	LongTasks     int     `json:"longTasks"`
}

// soakResult is what the soak row of page.mjs returns.
type soakResult struct {
	Turns struct {
		Turns        int       `json:"turns"`
		TotalMs      float64   `json:"totalMs"`
		P50Ms        float64   `json:"p50Ms"`
		P95Ms        float64   `json:"p95Ms"`
		MaxMs        float64   `json:"maxMs"`
		FailureCount int       `json:"failureCount"`
		Failures     []any     `json:"failures"`
		CompactionMs []float64 `json:"compactionMs"`
	} `json:"turns"`
	Reboot struct {
		BootMs float64 `json:"bootMs"`
	} `json:"reboot"`
	Load struct {
		ReplayMs float64        `json:"replayMs"`
		Error    any            `json:"error"`
		Updates  map[string]int `json:"updates"`
	} `json:"load"`
	Seed struct {
		SeedMs float64 `json:"seedMs"`
		Files  int     `json:"files"`
	} `json:"seed"`
	Grep struct {
		TurnMs     float64 `json:"turnMs"`
		StopReason string  `json:"stopReason"`
		Tools      []struct {
			Title string  `json:"title"`
			Ms    float64 `json:"ms"`
		} `json:"tools"`
	} `json:"grep"`
	Phases map[string]soakPhase `json:"phases"`
}

// soakTable renders the measurements as a Markdown table.
func soakTable(r soakResult) string {
	var b strings.Builder
	b.WriteString("| Phase | Page JS heap (MB) | Wasm memory (MB) | Longest main-thread task (ms) | Longest event-loop lag (ms) |\n|---|---|---|---|---|\n")
	for _, name := range []string{"boot", "turns", "reboot", "replay", "grep"} {
		p := r.Phases[name]
		fmt.Fprintf(&b, "| %s | %.1f | %.1f | %.0f | %.0f |\n", name, p.PageHeapMB, p.WasmMB, p.LongTaskMaxMs, p.LagMaxMs)
	}
	b.WriteString("\n| Measure | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Turns | %d in %.1f s (median %.0f ms, p95 %.0f ms, max %.0f ms) |\n", r.Turns.Turns, r.Turns.TotalMs/1000, r.Turns.P50Ms, r.Turns.P95Ms, r.Turns.MaxMs)
	fmt.Fprintf(&b, "| Automatic compactions | %d, each %.0f ms |\n", len(r.Turns.CompactionMs), r.Turns.CompactionMs)
	fmt.Fprintf(&b, "| Boot after the reload | %.0f ms |\n", r.Reboot.BootMs)
	fmt.Fprintf(&b, "| session/load replay | %.0f ms, %d updates |\n", r.Load.ReplayMs, sumCounts(r.Load.Updates))
	fmt.Fprintf(&b, "| Tree seeded | %d files in %.0f ms |\n", r.Seed.Files, r.Seed.SeedMs)
	for _, tool := range r.Grep.Tools {
		fmt.Fprintf(&b, "| grep over the tree | %.0f ms (turn %.0f ms) |\n", tool.Ms, r.Grep.TurnMs)
	}
	return b.String()
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

// TestBrowserSoak runs a long scripted session in headless Chromium: 300 turns
// with tool calls and two automatic compactions, a reload with session/load,
// and a grep over a tree of 3000 files. It measures and asserts only generous
// bounds (docs/design/acp-wasm.md, risk 10). It is a process-boundary test.
func TestBrowserSoak(t *testing.T) {
	skipShort(t)
	if os.Getenv(soakEnv) != "1" {
		t.Skip(soakEnv + "=1 enables the soak test")
	}
	node, embed := requireBrowserTools(t)
	t.Parallel()
	wasm, execJS, err := buildNodeModule()
	if err != nil {
		t.Fatal(err)
	}
	f := newBrowserFront(t, nil, soakSteps()...)
	k := &browserKit{t: t, wasm: wasm}
	page := browserPage(t, wasm, execJS, &k.awaits)
	spec := map[string]any{
		"name": "soak", "kind": "soak", "baseURL": f.URL, "timeoutMs": 8 * 60 * 1000,
		"wasmExec": "wasm_exec_probe.js", "turns": soakTurns, "dirs": soakDirs, "perDir": soakPerDir,
		"settings": map[string]any{"context_window_tokens": soakWindow, "compaction_keep_turns": 2},
	}
	res := runBrowserWithin(t, 10*time.Minute, node, embed, page.URL, wasm, []map[string]any{spec})["soak"]
	k.noFailure(t, res)
	var r soakResult
	rowData(t, res, &r)

	table := soakTable(r)
	t.Log("\n" + table)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		out, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		_, _ = io.WriteString(out, "### Browser soak\n\n"+table)
	}

	if r.Turns.FailureCount != 0 || r.Turns.Turns != soakTurns {
		t.Errorf("turns = %d, failures = %d %v", r.Turns.Turns, r.Turns.FailureCount, r.Turns.Failures)
	}
	if len(r.Turns.CompactionMs) < 1 {
		t.Errorf("automatic compactions = %d, want at least 1", len(r.Turns.CompactionMs))
	}
	if r.Load.Error != nil {
		t.Errorf("session/load: %v", r.Load.Error)
	}
	if got := time.Duration(r.Load.ReplayMs * float64(time.Millisecond)); got > soakReplayBound {
		t.Errorf("replay took %s, want under %s", got, soakReplayBound)
	}
	if r.Load.Updates["user_message_chunk"] < soakTurns {
		t.Errorf("replay sent %d user messages, want at least %d", r.Load.Updates["user_message_chunk"], soakTurns)
	}
	if r.Grep.StopReason != "end_turn" || len(r.Grep.Tools) != 1 {
		t.Errorf("grep turn = %q with %d tool calls, want end_turn and 1", r.Grep.StopReason, len(r.Grep.Tools))
	}
	for name, p := range r.Phases {
		if p.PageHeapMB > soakMemoryBound || p.WasmMB > soakMemoryBound {
			t.Errorf("phase %s: memory %+v above %.0f MB", name, p, soakMemoryBound)
		}
	}
	found := 0
	for _, req := range f.fake.Requests() {
		if soakLastIsResult(req) && strings.HasPrefix(req.LastUserText(), "soak grep") {
			found = strings.Count(requestText(req), "needle_")
		}
	}
	if found != soakDirs {
		t.Errorf("the grep result that the model saw holds %d matches, want %d", found, soakDirs)
	}
}
