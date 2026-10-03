package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
)

// fixtureConfigDir is the CLAUDE_CONFIG_DIR of the recorded mirror fixtures.
const fixtureConfigDir = "/home/u/cfg"

// replayMirror replays the frames of the fixture named by
// FAKE_CLAUDE_MIRROR_FIXTURE. Each transcript_mirror frame appends its
// entries to its file under CLAUDE_CONFIG_DIR, as --session-mirror does.
// FAKE_CLAUDE_MIRROR_SEEN receives the files found there at start, and
// FAKE_CLAUDE_MIRROR_HANG_AFTER=n hangs after n mirror frames.
func replayMirror(f *fake) bool {
	cfgDir := os.Getenv("CLAUDE_CONFIG_DIR")
	recordSeen(cfgDir)
	raw, err := os.ReadFile(os.Getenv("FAKE_CLAUDE_MIRROR_FIXTURE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	hangAfter := -1
	if v := os.Getenv("FAKE_CLAUDE_MIRROR_HANG_AFTER"); v != "" {
		hangAfter, _ = strconv.Atoi(v)
	}
	frames := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var frame struct {
			Type     string            `json:"type"`
			FilePath string            `json:"filePath"`
			Entries  []json.RawMessage `json:"entries"`
		}
		if json.Unmarshal([]byte(line), &frame) != nil {
			continue
		}
		if frame.Type != "transcript_mirror" {
			f.emitRaw(line)
			continue
		}
		if frames == hangAfter {
			hang(f)
		}
		frames++
		path := cfgDir + strings.TrimPrefix(frame.FilePath, fixtureConfigDir)
		appendEntries(path, frame.Entries)
		f.emitVerbatim(obj{"type": "transcript_mirror", "filePath": path, "entries": frame.Entries})
	}
	return true
}

func (f *fake) emitRaw(line string) {
	_, _ = fmt.Fprintln(f.out, line)
	_ = f.out.Flush()
}

// emitVerbatim keeps the bytes of each entry: the CLI does not escape HTML.
func (f *fake) emitVerbatim(v obj) {
	enc := json.NewEncoder(f.out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	_ = f.out.Flush()
}

func recordSeen(cfgDir string) {
	seenPath := os.Getenv("FAKE_CLAUDE_MIRROR_SEEN")
	if seenPath == "" {
		return
	}
	files := map[string]string{}
	_ = filepath.WalkDir(cfgDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(cfgDir, p)
			files[rel] = string(b)
		}
		return nil
	})
	b, _ := json.Marshal(obj{"config_dir": cfgDir, "files": files})
	appendFile(seenPath, string(b)+"\n")
}

func appendEntries(path string, entries []json.RawMessage) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	var sb strings.Builder
	for _, e := range entries {
		sb.Write(e)
		sb.WriteByte('\n')
	}
	appendFile(path, sb.String())
}

// questionMirrorFrame emits one transcript_mirror frame in "question" mode
// when FAKE_CLAUDE_QUESTION_MIRROR is set.
func questionMirrorFrame(f *fake, tag string) {
	if os.Getenv("FAKE_CLAUDE_QUESTION_MIRROR") == "" {
		return
	}
	f.emit(obj{"type": "transcript_mirror",
		"filePath": filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "p", "sess.jsonl"),
		"entries":  []obj{{"tag": tag}}})
}

// initExtras adds the tools of FAKE_CLAUDE_INIT_TOOLS to the init frame.
func initExtras(init obj) {
	if raw, ok := os.LookupEnv("FAKE_CLAUDE_INIT_TOOLS"); ok {
		init["tools"] = json.RawMessage(raw)
	}
}

// logEnv writes the environment to FAKE_CLAUDE_ENV_LOG.
func logEnv() {
	if path := os.Getenv("FAKE_CLAUDE_ENV_LOG"); path != "" {
		b, _ := json.Marshal(os.Environ())
		_ = os.WriteFile(path, b, 0o644)
	}
}

// logInterrupt appends "interrupt" to FAKE_CLAUDE_SIGNAL_LOG on SIGINT, then
// prints the onInterrupt frames of the mode and the error result of the
// interrupted turn, and exits.
func logInterrupt() {
	path := os.Getenv("FAKE_CLAUDE_SIGNAL_LOG")
	if path == "" {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		appendFile(path, "interrupt\n")
		for _, v := range append(onInterrupt[os.Getenv("FAKE_CLAUDE_MODE")], result("error_during_execution", true, "", 7, 2)) {
			b, _ := json.Marshal(v)
			fmt.Println(string(b))
		}
		os.Exit(130)
	}()
}
