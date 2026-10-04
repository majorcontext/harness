package toolresult_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/protocol"
)

var bg = context.Background()

func TestMask(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"AWS_SECRET_ACCESS_KEY=abcdefgh123", "AWS_SECRET_ACCESS_KEY=***"},
		{"password: hunter2hunter2\nok", "password: ***\nok"},
		{`{"api_key": "sk-123"}`, `{"api_key": "***"}`},
		{"Authorization: Bearer abcdefgh.ijk", "Authorization: Bearer ***"},
		{`export TOKEN="secret value"`, `export TOKEN="***"`},
		{"TOKEN='a b c'", "TOKEN='***'"},
		{"token:=lexer.Next()", "token:=lexer.Next()"},
		{"https://x/?a=1&token=abcdefgh1234&b=2", "https://x/?a=1&token=***&b=2"},
		{"token: short", "token: short"},
		{"no keys here", "no keys here"},
	} {
		if got := toolresult.Mask(tc.in); got != tc.want {
			t.Errorf("Mask(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// turn runs one turn of a WorkDir runtime on st that makes each call in
// order, and returns the tool result texts.
func turn(t *testing.T, cfg config.Config, calls ...map[string]any) []string {
	t.Helper()
	return allowedTurn(t, nil, cfg, calls...)
}

// allowedTurn is turn in a session that allows only the tools named.
func allowedTurn(t *testing.T, allowed []string, cfg config.Config, calls ...map[string]any) []string {
	t.Helper()
	var steps []harnesstest.Step
	for i, in := range calls {
		name := "bash"
		switch {
		case in["handle"] != nil:
			name = toolresult.ToolName
		case in["path"] != nil:
			name = "read_file"
		}
		steps = append(steps, harnesstest.Step{Name: fmt.Sprint(i), Match: results(i),
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: fmt.Sprint("c", i), Name: name, Input: in}}}})
	}
	steps = append(steps, harnesstest.Step{Name: "done", Match: results(len(calls)), Reply: harnesstest.Reply{Text: "done"}})
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	cfg.Providers = map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
		BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: t.TempDir(), Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(bg) }()
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5", AllowedTools: allowed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(bg, protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	var out []string
	for e, err := range sess.Events(bg, 0) {
		if err != nil || e.Kind == "turn.ended" {
			break
		}
		var item struct {
			Message struct {
				Role  string `json:"role"`
				Parts []struct {
					Text    string `json:"text"`
					IsError bool   `json:"is_error"`
				} `json:"parts"`
			} `json:"message"`
		}
		if e.Kind == "item.completed" && json.Unmarshal(e.Data, &item) == nil && item.Message.Role == "tool" {
			out = append(out, map[bool]string{true: "ERR "}[item.Message.Parts[0].IsError]+item.Message.Parts[0].Text)
		}
	}
	return out
}

func results(n int) harnesstest.Matcher {
	return func(r harnesstest.Request) bool {
		got := 0
		for _, m := range r.Messages {
			for _, p := range m.Parts {
				if p.Kind == "tool_result" {
					got++
				}
			}
		}
		return got == n
	}
}

func lines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

func bash(cmd string) map[string]any { return map[string]any{"command": cmd} }

func read(kv ...any) map[string]any {
	in := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		in[kv[i].(string)] = kv[i+1]
	}
	return in
}

// seq 1 5000 writes 23893 bytes in 5000 lines.
const seq = "seq 1 5000"

// big writes 2000 lines of 2000 two-byte runes. read_file returns 8014892
// bytes of it, which alone exceed the session budget.
const big = `l=$(printf 'é%.0s' $(seq 2000)); for i in $(seq 2000); do echo "$l"; done > big.txt`

func TestRetention(t *testing.T) {
	got := turn(t, config.Config{},
		bash("printf 'TOKEN=abcdefgh1234\\n'; "+seq),
		read("handle", "trh_1", "offset", 5000, "limit", 5),
		read("handle", "trh_1", "search", "TOKEN"),
		read("handle", "trh_1", "offset", 1, "max_bytes", 300),
		read("handle", "trh_9"),
		read("handle", "trh_01"),
		bash(big),
		read("path", "big.txt"),
		bash(seq),
		bash("seq 1 3"),
	)
	header := `[tool result retained: handle=trh_1 tool=bash bytes=23903 lines=5001 preview_bytes=16384 — read the rest with read_tool_result(handle="trh_1")]` + "\nTOKEN=***\n1\n2\n"
	want := []string{
		header,
		"trh_1 (tool=bash, 23903 bytes, 5001 lines) lines 5000-5004:\n4999\n5000\n",
		"trh_1 (tool=bash, 23903 bytes, 5001 lines) lines matching \"TOKEN\":\n1: TOKEN=***\n",
		// 56 preamble bytes and 116 bytes of lines fill 300 less the 128 kept for the notice.
		"trh_1 (tool=bash, 23903 bytes, 5001 lines) lines 1-200:\nTOKEN=***\n" + lines(1, 38) + "[truncated at 300 bytes; continue with offset=40]\n",
		`ERR read_tool_result: unknown handle "trh_9" (this session's handles: trh_1)`,
		`ERR read_tool_result: malformed handle "trh_01" (want trh_N, e.g. trh_1)`,
		"",
		"[tool result truncated: tool=read_file bytes=8014892 preview_bytes=16384 — retaining this result would exceed the per-session retention budget; its remainder is discarded irrecoverably, though a smaller result later this session may still be retained]\n1→éé",
		`[tool result retained: handle=trh_2 tool=bash bytes=23893 lines=5000 preview_bytes=16384 — read the rest with read_tool_result(handle="trh_2")]` + "\n1\n2\n",
		"1\n2\n3\n",
	}
	if len(got) != len(want) {
		t.Fatalf("results = %d %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) || i != 0 && i != 7 && i != 8 && got[i] != want[i] {
			t.Errorf("result %d = %.300q, want %.300q", i, got[i], want[i])
		}
	}
	if n := len(got[0]) - strings.Index(got[0], "\n") - 1; n != 16384 {
		t.Errorf("preview = %d bytes, want 16384", n)
	}
}

type replica struct{ st harness.Store }

func (r replica) Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error) {
	return harness.ApplySync(ctx, r.st, b)
}

func TestRetainedResultsAcrossTurns(t *testing.T) {
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{},
		harnesstest.Step{Name: "bash", Match: harnesstest.LastUserText("one"),
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "bash", Input: bash(seq)}}}},
		harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "ok"}},
		harnesstest.Step{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "ok"}},
		harnesstest.Step{Name: "summary", Match: harnesstest.SystemContains("summarizing"), Reply: harnesstest.Reply{Text: "summary"}})
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	cfg := config.Config{CompactionKeepTurns: 1, Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
		BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}}}
	copy := harness.NewMemStore()
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Sync: replica{copy}, WorkDir: t.TempDir(), Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(bg) }()
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"one", "two"} {
		head := sess.View().HeadSeq
		if _, err := sess.Submit(bg, protocol.Input{ID: in, Parts: []protocol.Part{{Type: protocol.PartText, Text: in}}}); err != nil {
			t.Fatal(err)
		}
		for e, err := range sess.Events(bg, head) {
			if err != nil || e.Kind == "turn.ended" {
				break
			}
		}
	}
	if _, err := sess.Compact(bg, protocol.Compact{}); err != nil {
		t.Fatal(err)
	}
	if got := s.Requests()[1].Messages; !strings.HasPrefix(got[len(got)-1].Parts[0].Text, "[tool result retained: handle=trh_1 ") {
		t.Errorf("the model call after the tool got %.100q, want the preview", got[len(got)-1].Parts[0].Text)
	}
	var summary string
	for e, err := range sess.Events(bg, 0) {
		if err != nil {
			t.Fatal(err)
		}
		var c struct{ Summary string }
		if e.Kind == "compaction.applied" && json.Unmarshal(e.Data, &c) == nil {
			summary = c.Summary
			break
		}
	}
	want := "summary\n\n[retained tool results (this index is machine-generated, not part of the summary above):\n" +
		fmt.Sprintf("  trh_1 tool=bash bytes=23893 lines=5000 head=%q (still readable via read_tool_result)\n]", strings.TrimSuffix(lines(1, 30), "\n"))
	if !strings.HasSuffix(summary, want) {
		t.Errorf("compaction summary = %q, want it to end with the index %q", summary, want)
	}
	rc, err := copy.GetBlob(bg, "s1", "trh_1-2")
	if err != nil {
		t.Fatalf("the Sync replica has no retained blob: %v", err)
	}
	_ = rc.Close()
}

func TestRetentionKeepsTheHookedResult(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pluginfixture")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/majorcontext/harness/harnesstest/pluginfixture").CombinedOutput(); err != nil {
		t.Fatalf("go build pluginfixture: %v\n%s", err, out)
	}
	got := turn(t, config.Config{Plugins: []config.PluginSpec{{Name: "fixture", Command: []string{bin}, Config: []byte(`{}`)}}}, bash(seq))
	want := `[tool result retained: handle=trh_1 tool=bash bytes=23909 lines=5000 preview_bytes=16384 — read the rest with read_tool_result(handle="trh_1")]` + "\nafter-hook saw: 1\n2\n"
	if len(got) != 1 || !strings.HasPrefix(got[0], want) {
		t.Fatalf("results = %.200q, want the retained text of the after hook %q", got, want)
	}
}

func TestReadBoundsTheDefaultBudget(t *testing.T) {
	got := turn(t, config.Config{}, bash(seq), read("handle", "trh_1", "search", strings.Repeat("a", 20000)))
	if len(got) != 2 || !strings.HasPrefix(got[1], "ERR read_tool_result: max_bytes 16384 is below the minimum ") {
		t.Errorf("result = %.200q, want a max_bytes error", got)
	}
}

func TestNoRetentionWithoutTheReader(t *testing.T) {
	got := allowedTurn(t, []string{"bash"}, config.Config{}, bash(seq))
	if len(got) != 1 || got[0] != lines(1, 5000) {
		t.Errorf("result = %.200q, want the whole result inline", got)
	}
}
