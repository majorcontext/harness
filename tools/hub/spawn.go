// Package hub implements `harness hub`: a local, single-operator control
// surface over a fleet of headless harness boxes. See hub.go for the server
// and index.html for the page itself.
package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// boxNameEnv is the environment variable the spawn command's own process
// sees the hub-chosen (or operator-chosen) box NAME in — see AGENTS.md's
// spawn contract section and docs/design/fleet-model.md §8. Deployment
// tooling invoked by -spawn-command reads this to derive per-name storage
// (e.g. HARNESS_SESSION_DIR); harness's own code never reads it.
const boxNameEnv = "HARNESS_HUB_BOX_NAME"

// spawnEvent is one frame of the /spawn SSE stream, JSON-encoded as the
// `data:` payload. This is the entire spawn-output contract described in
// AGENTS.md: a "stdout" event per line of the spawn command's combined
// stdout+stderr, and exactly one terminal "done" event carrying the exit
// status plus whatever TUNNEL_URL / RUN_TOKEN lines were found along the
// way. The page needs nothing else to add the new box to its own state.
type spawnEvent struct {
	Type      string `json:"type"` // "stdout" or "done"
	Line      string `json:"line,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
	TunnelURL string `json:"tunnel_url,omitempty"`
	RunToken  string `json:"run_token,omitempty"`
	// PortURLs collects every `PORT_URL_<port>=<url>` line found in the
	// spawn command's output (same trim/one-line-only rule as TUNNEL_URL/
	// RUN_TOKEN below), keyed by the port string. Omitted entirely when no
	// such line appeared — the page's box state (port_urls) only ever
	// gains entries a spawn (or hand-edit) actually produced. See DELIVERABLE
	// (3)'s process-strip preview links.
	PortURLs map[string]string `json:"port_urls,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// marshal encodes ev as a single SSE frame ("data: ...\n\n"). It never
// fails in practice (spawnEvent has no unmarshalable fields), but a JSON
// error is folded into the frame rather than panicking or dropping it.
func (ev spawnEvent) marshal() []byte {
	b, err := json.Marshal(ev)
	if err != nil {
		b = []byte(`{"type":"done","error":"internal: encoding spawn event: ` + err.Error() + `"}`)
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// tunnelURLPattern and runTokenPattern match the two contract lines
// anywhere in the spawn command's combined stdout+stderr, tolerating
// leading/trailing whitespace and any surrounding log prefix on the same
// line is NOT stripped — the line must consist of exactly "KEY=value" once
// trimmed, so a logger that prefixes timestamps must emit the marker on its
// own line.
var (
	tunnelURLPattern = regexp.MustCompile(`^TUNNEL_URL=(.+)$`)
	runTokenPattern  = regexp.MustCompile(`^RUN_TOKEN=(.+)$`)
	portURLPattern   = regexp.MustCompile(`^PORT_URL_([0-9]+)=(.+)$`)
)

// runSpawn execs command via `sh -c`, streaming each combined stdout/stderr
// line to emit as a "stdout" spawnEvent and scanning every line against the
// TUNNEL_URL=/RUN_TOKEN=/PORT_URL_<port>= contract. It always finishes by
// calling emit exactly once more with a "done" event — carrying the exit
// code and whatever contract values were found, or an Error string if the
// command could not even be started. Canceling ctx kills the process
// (SIGKILL via exec.CommandContext) and runSpawn returns promptly once the
// process actually exits; it does not return early on cancellation, so the
// final "done" event is always sent.
//
// name, when non-empty, is set as HARNESS_HUB_BOX_NAME in the spawned
// command's own environment (see boxNameEnv above) — the page's generated
// slug, or the same name again on a Respawn/ADOPT (see fleet-model.md §4).
// Empty name leaves the environment exactly as os/exec's default (no
// HARNESS_HUB_BOX_NAME at all), not an empty-valued entry.
func runSpawn(ctx context.Context, command, name string, emit func(spawnEvent)) {
	if strings.TrimSpace(command) == "" {
		emit(spawnEvent{Type: "done", Error: "no spawn command configured (set -spawn-command or HARNESS_HUB_SPAWN)"})
		return
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if name != "" {
		cmd.Env = append(os.Environ(), boxNameEnv+"="+name)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emit(spawnEvent{Type: "done", Error: fmt.Sprintf("spawn: %v", err)})
		return
	}
	cmd.Stderr = cmd.Stdout // combined stream, in the order it's written

	if err := cmd.Start(); err != nil {
		emit(spawnEvent{Type: "done", Error: fmt.Sprintf("spawn: %v", err)})
		return
	}

	var tunnelURL, runToken string
	var portURLs map[string]string
	scanner := bufio.NewScanner(stdout)
	// Spawn scripts may print long single lines (progress bars, etc.); grow
	// past bufio's 64KiB default rather than truncating or erroring out.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		emit(spawnEvent{Type: "stdout", Line: line})
		trimmed := strings.TrimSpace(line)
		if m := tunnelURLPattern.FindStringSubmatch(trimmed); m != nil {
			tunnelURL = strings.TrimSpace(m[1])
		}
		if m := runTokenPattern.FindStringSubmatch(trimmed); m != nil {
			runToken = strings.TrimSpace(m[1])
		}
		if m := portURLPattern.FindStringSubmatch(trimmed); m != nil {
			if portURLs == nil {
				portURLs = make(map[string]string)
			}
			portURLs[m[1]] = strings.TrimSpace(m[2])
		}
	}
	scanErr := scanner.Err()

	waitErr := cmd.Wait()

	done := spawnEvent{Type: "done", TunnelURL: tunnelURL, RunToken: runToken, PortURLs: portURLs}
	if cmd.ProcessState != nil {
		done.ExitCode = cmd.ProcessState.ExitCode()
	}
	switch {
	case scanErr != nil && scanErr != io.EOF:
		done.Error = fmt.Sprintf("reading spawn output: %v", scanErr)
	case waitErr != nil:
		done.Error = waitErr.Error()
	}
	emit(done)
}
