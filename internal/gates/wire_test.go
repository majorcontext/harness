package gates

import (
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/wirescrub"
)

var recordings = map[string]struct {
	wire  Wire
	files []string
}{
	"anthropic":  {AnthropicWire, []string{"anthropic.text.sse", "anthropic.tool.sse", "anthropic.tool-result.sse", "anthropic.thinking.sse", "anthropic.error.json"}},
	"chat":       {ChatWire, []string{"chat.text.sse", "chat.tool.sse", "chat.tool-result.sse", "chat.error.json"}},
	"responses":  {ResponsesWire, []string{"responses.text.sse", "responses.tool.sse", "responses.tool-result.sse", "responses.incomplete.sse", "responses.error.json"}},
	"claudecode": {ClaudeCodeWire, []string{"claudecode.tool.jsonl", "claudecode.interrupt.jsonl", "claudecode.error.jsonl"}},
}

func loadAllowed(t *testing.T) *Allowed {
	t.Helper()
	data, err := os.ReadFile("../../testdata/wire/allowed.txt")
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseAllowed(data)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func realShape(t *testing.T, name string) Shape {
	t.Helper()
	rec := recordings[name]
	s, err := RecordedShape(os.DirFS("../../testdata/wire"), rec.wire, rec.files)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRecordingsHoldNoSecretOrIdentifier(t *testing.T) {
	for name, rec := range recordings {
		for _, f := range rec.files {
			data, err := os.ReadFile("../../testdata/wire/" + f)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if leaks := wirescrub.Leaks(data); len(leaks) > 0 {
				t.Errorf("%s holds %s", f, strings.Join(leaks, "; "))
			}
		}
	}
}

func TestWireDoublesMatchTheRealWire(t *testing.T) {
	allowed := loadAllowed(t)
	t.Run("parsers read only fields the real wire sends", func(t *testing.T) {
		reads, err := ParserReads(os.DirFS("../.."))
		if err != nil {
			t.Fatal(err)
		}
		for name := range recordings {
			for _, d := range ParserDifferences(name, realShape(t, name), reads, allowed) {
				t.Error(d)
			}
		}
	})
	t.Run("fakes emit only what the real wire sends", func(t *testing.T) {
		fakes := map[string]Shape{
			"anthropic":  fakeAnthropicShape(t),
			"chat":       fakeChatShape(t),
			"responses":  fakeResponsesShape(t),
			"claudecode": fakeClaudeShape(t),
		}
		for name, fake := range fakes {
			for _, d := range FakeDifferences(name, realShape(t, name), fake, allowed) {
				t.Error(d)
			}
		}
	})
	if t.Failed() {
		return
	}
	for _, line := range allowed.Unused() {
		t.Errorf("testdata/wire/allowed.txt: no check needs %q", line)
	}
}
