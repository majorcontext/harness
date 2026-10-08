package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type frameShapes map[string]map[string]string

func kindOfFrame(v map[string]any) string {
	kind, _ := v["type"].(string)
	if sub, ok := v["subtype"].(string); ok {
		kind += "/" + sub
	}
	return kind
}

func elementName(v any) string {
	if m, ok := v.(map[string]any); ok {
		if t, ok := m["type"].(string); ok {
			return "[" + t + "]"
		}
	}
	return "[]"
}

func shapePaths(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			out[prefix] = "object"
		}
		for k, e := range x {
			shapePaths(prefix+"."+k, e, out)
		}
	case []any:
		if len(x) == 0 {
			out[prefix+"[]"] = "array"
		}
		for _, e := range x {
			shapePaths(prefix+elementName(e), e, out)
		}
	default:
		out[prefix] = kindOf(v)
	}
}

func readShapes(t *testing.T, lines [][]byte, into frameShapes) {
	t.Helper()
	for _, line := range lines {
		v, err := decodeJSON(line)
		if err != nil {
			t.Fatalf("frame %s: %v", line, err)
		}
		frame, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("frame %s is not an object", line)
		}
		kind := kindOfFrame(frame)
		if into[kind] == nil {
			into[kind] = map[string]string{}
		}
		shapePaths("", frame, into[kind])
	}
}

func recordedFrames(t *testing.T) frameShapes {
	t.Helper()
	shapes := frameShapes{}
	for _, name := range []string{"run1.stdout.jsonl", "run2.stdout.jsonl"} {
		f, err := os.Open(filepath.Join("..", "harnesstest", "fakeclaude", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		var lines [][]byte
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			lines = append(lines, bytes.Clone(sc.Bytes()))
		}
		_ = f.Close()
		if sc.Err() != nil {
			t.Fatal(sc.Err())
		}
		readShapes(t, lines, shapes)
	}
	return shapes
}

func fakeClaudeFrames(t *testing.T) frameShapes {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	cmd := exec.CommandContext(ctx, fakeClaudePath())
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fakeclaude: %v", err)
	}
	shapes := frameShapes{}
	readShapes(t, bytes.Split(bytes.TrimSpace(out), []byte("\n")), shapes)
	return shapes
}

func elementNames(paths map[string]string) map[string]bool {
	names := map[string]bool{}
	for p := range paths {
		for _, seg := range strings.Split(p, "[")[1:] {
			name, _, _ := strings.Cut(seg, "]")
			names["["+name+"]"] = true
		}
	}
	return names
}

func underRecordedElements(path string, recorded map[string]bool) bool {
	for _, seg := range strings.Split(path, "[")[1:] {
		name, _, _ := strings.Cut(seg, "]")
		if !recorded["["+name+"]"] {
			return false
		}
	}
	return true
}

func shapeDeltas(recorded, fake frameShapes) (deltas []string) {
	for kind, got := range fake {
		want, known := recorded[kind]
		if !known {
			continue
		}
		names := elementNames(want)
		for p, k := range want {
			switch gk, ok := got[p]; {
			case !ok:
				deltas = append(deltas, fmt.Sprintf("%s lacks %s", kind, p))
			case gk != k:
				deltas = append(deltas, fmt.Sprintf("%s has %s as %s, recorded %s", kind, p, gk, k))
			}
		}
		for p := range got {
			if _, ok := want[p]; !ok && underRecordedElements(p, names) {
				deltas = append(deltas, fmt.Sprintf("%s adds %s", kind, p))
			}
		}
	}
	slices.Sort(deltas)
	return deltas
}

const (
	recordingTrimmed = "2026-10-08: the recording lacks it, so nothing shows that the real CLI sends it"
	documentedGap    = "2026-10-08: fakeclaude documents that its normal turn omits it (main.go)"
)

var fakeClaudeDeltas = map[string]string{
	"result/success lacks .num_turns":                        documentedGap,
	"result/success lacks .session_id":                       documentedGap,
	"system/init lacks .model":                               "2026-10-08: only the modes that need a model set it",
	"system/init lacks .tools[]":                             "2026-10-08: only FAKE_CLAUDE_INIT_TOOLS sets the tool list",
	"result/success adds .duration_ms":                       recordingTrimmed,
	"result/success adds .total_cost_usd":                    recordingTrimmed,
	"result/success adds .ttft_ms":                           recordingTrimmed,
	"result/success adds .usage.cache_creation_input_tokens": recordingTrimmed,
	"result/success adds .usage.cache_read_input_tokens":     recordingTrimmed,
}

func TestContractFakeClaudeFramesMatchTheRecordedCLI(t *testing.T) {
	skipShort(t)
	t.Parallel()
	recorded, fake := recordedFrames(t), fakeClaudeFrames(t)
	for _, kind := range []string{"system/init", "assistant", "result/success"} {
		if recorded[kind] == nil || fake[kind] == nil {
			t.Fatalf("frame kind %s: recorded %v, emitted by fakeclaude %v; the check compares nothing", kind, recorded[kind] != nil, fake[kind] != nil)
		}
	}
	deltas := shapeDeltas(recorded, fake)
	for _, d := range deltas {
		if _, ok := fakeClaudeDeltas[d]; !ok {
			t.Errorf("fakeclaude differs from the recorded CLI: %s", d)
		}
	}
	for d := range fakeClaudeDeltas {
		if !slices.Contains(deltas, d) {
			t.Errorf("fakeclaude no longer differs as listed: %s; delete the entry", d)
		}
	}
}
