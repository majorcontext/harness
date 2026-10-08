// Command wirerecord records real model streams and scrubs them into
// testdata/wire. It makes real model calls, so it runs only when
// HARNESS_RECORD_WIRE=1. Keys come from ANTHROPIC_API_KEY and OPENAI_API_KEY;
// the Claude Code CLI uses its own login.
//
//	HARNESS_RECORD_WIRE=1 go run ./internal/wirerecord -out testdata/wire [-only anthropic,chat,responses,claudecode]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/majorcontext/harness/internal/wirescrub"
)

const (
	anthropicModel = "claude-haiku-4-5"
	chatModel      = "gpt-4.1-nano"
	responsesModel = "gpt-5-nano"
	claudeModel    = "claude-haiku-4-5"
)

type recorder struct {
	out      string
	scrubber *wirescrub.Scrubber
}

func (r *recorder) save(name string, data []byte) error {
	clean := r.scrubber.Bytes(data)
	if leaks := wirescrub.Leaks(clean); len(leaks) > 0 {
		return fmt.Errorf("%s still holds: %s", name, strings.Join(leaks, "; "))
	}
	return os.WriteFile(filepath.Join(r.out, name), clean, 0o644)
}

func main() {
	out := flag.String("out", "testdata/wire", "output directory")
	only := flag.String("only", "anthropic,chat,responses,claudecode", "comma-separated wires to record")
	flag.Parse()
	if os.Getenv("HARNESS_RECORD_WIRE") != "1" {
		fmt.Fprintln(os.Stderr, "wirerecord: set HARNESS_RECORD_WIRE=1 to make real model calls")
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	work, err := os.MkdirTemp("", "wirerecord-")
	if err != nil {
		fatal(err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	r := &recorder{out: *out, scrubber: wirescrub.New(work)}
	runners := map[string]func(*recorder, string) error{
		"anthropic": recordAnthropic, "chat": recordChat, "responses": recordResponses, "claudecode": recordClaudeCode,
	}
	for _, name := range strings.Split(*only, ",") {
		run, ok := runners[name]
		if !ok {
			fatal(fmt.Errorf("unknown wire %q", name))
		}
		if err := run(r, work); err != nil {
			fatal(fmt.Errorf("%s: %w", name, err))
		}
		fmt.Fprintln(os.Stderr, "recorded", name)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "wirerecord:", err)
	os.Exit(1)
}
