package main

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// transcripts is a CLI that keeps the IDs of the sessions that it started in
// the file FAKE_CLAUDE_STATE.sessions, its stand-in for the transcripts of its
// config directory. A run that resumes nothing starts the session
// fake-session-<spawn>. A resume of a listed session runs the turn on it. A
// resume of any other session fails as the CLI fails it when the transcript
// is gone: one line on stderr, then a failed result and no init frame.
// FAKE_CLAUDE_RESUME_ERROR replaces the text of that failure.
func transcripts(f *fake) bool {
	path := os.Getenv("FAKE_CLAUDE_STATE") + ".sessions"
	i := slices.Index(os.Args, "--resume")
	if i < 0 || i+1 >= len(os.Args) {
		f.sessionID = "fake-session-" + strconv.Itoa(f.spawn)
		appendFile(path, f.sessionID+"\n")
		return false
	}
	id := os.Args[i+1]
	known, _ := os.ReadFile(path)
	if slices.Contains(strings.Fields(string(known)), id) {
		f.sessionID = id
		return false
	}
	text := cmp.Or(os.Getenv("FAKE_CLAUDE_RESUME_ERROR"), "No conversation found with session ID: "+id)
	fmt.Fprintln(os.Stderr, text)
	f.emit(failed(0, 0, text).set("num_turns", 0).set("session_id", id).set("total_cost_usd", 0))
	return true
}
