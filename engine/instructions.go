// Project-instruction injection: an AGENTS.md discovered near the working
// directory is appended to the system prompt so repo-specific guidance
// applies without the user having to ask for it.
//
// Format and discovery follow the agents.md convention
// (https://agents.md/): the file is schema-less standard Markdown — the agent
// simply parses the text, using no fixed headings — and the "closest" file to
// the working directory wins. Our walk-up-from-WorkDir search implements that
// closest-wins precedence: the first AGENTS.md (or AGENT.md fallback) found
// while ascending toward the git/filesystem root is used. os.ReadFile follows
// symlinks, so the spec's `ln -s AGENTS.md AGENT.md` compatibility setup works
// transparently.
//
// Discovery touches disk, so fresh sessions run it in bounded asynchronous
// startup prewarm after final construction. Loaded sessions run it lazily on
// the first Prompt. The result is cached for the session's life. Instructions
// are never written to the session log. A loaded session reads them fresh.

package engine

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// instructionsFilenames are the project-instruction file names checked in each
// directory, in preference order (AGENTS.md wins over the singular AGENT.md).
var instructionsFilenames = []string{"AGENTS.md", "AGENT.md"}

// defaultMaxInstructionsBytes caps how much of an instruction file is read
// into the system prompt when InstructionsConfig.MaxBytes is unset. Content
// beyond the cap is dropped and replaced with the marker
// formatTruncationMarker builds.
const defaultMaxInstructionsBytes = 64 * 1024

// InstructionsConfig controls project-instruction (AGENTS.md) injection. As a
// field on Config it has three meaningful states:
//
//   - nil: the default — auto-discover AGENTS.md by walking up from WorkDir.
//   - &InstructionsConfig{Disabled: true}: no injection.
//   - &InstructionsConfig{Path: "..."}: load that specific file instead of
//     searching (a missing override file simply yields no segment).
type InstructionsConfig struct {
	// Disabled turns off injection entirely.
	Disabled bool
	// Path, when non-empty, is a specific instruction file to load instead of
	// auto-discovering AGENTS.md.
	Path string
	// MaxBytes caps the instruction bytes injected into the system prompt.
	// Zero (the zero value) takes defaultMaxInstructionsBytes (64 KiB); a
	// positive value sets the cap; a NEGATIVE value disables the cap, so the
	// whole file is injected however large it is. Truncation is always loud —
	// see truncateInstructions.
	MaxBytes int
	// Mode selects how an OVERSIZE file is rendered: InstructionsModeAuto
	// (the zero value) splits it into a head plus an outline of the sections
	// the head does not carry, and InstructionsModeFull keeps the
	// head-plus-marker rendering. See engine/instructions_outline.go.
	Mode InstructionsMode
}

// resolveInstructionsMaxBytes reports the instruction byte cap for ic. A nil
// ic, or a zero MaxBytes, takes the default; a negative MaxBytes is passed
// through unchanged and disables the cap.
func resolveInstructionsMaxBytes(ic *InstructionsConfig) int {
	if ic == nil || ic.MaxBytes == 0 {
		return defaultMaxInstructionsBytes
	}
	return ic.MaxBytes
}

// formatTruncationMarker builds the in-band marker that replaces the dropped
// tail of an oversize instruction file. The marker is VISIBLE to the model on
// purpose: an instruction file cut in half without a word is
// indistinguishable from a file that simply ends there, so a model follows a
// half specification and believes it read the whole one. The marker names the
// path, the three byte counts, and the tool that reads the rest, so the model
// can recover the dropped content itself.
//
// The `[... ... ...]` bracket form is this repository's own marker
// convention (see engine/messagepage.go and engine/toolresult_tool.go). It
// serves the same purpose as the fx harness's inline <context_limit> markers:
// a truncation the reader can see.
//
// path is the ABSOLUTE path, while the segment header above it
// (formatInstructions) shows the short display path. The two differ on
// purpose: the header names the file for a reader, the marker names an
// argument the model gives to read_file, and an absolute path resolves the
// same from any working directory.
func formatTruncationMarker(path string, total, kept int) string {
	return fmt.Sprintf(
		"[... truncated: %s is %d bytes. The first %d bytes are above. %d bytes are not shown. Read the full file with the read_file tool. ...]",
		path, total, kept, total-kept,
	)
}

// loadInstructions searches from workDir upward for AGENTS.md (falling back to
// AGENT.md) and returns its (possibly truncated) content plus a display path
// relative to workDir. The walk stops at the first directory containing a .git
// entry — that directory is checked for an instructions file before stopping —
// or at the filesystem root. A missing file yields empty strings and no error.
// maxBytes is the resolved byte cap (see resolveInstructionsMaxBytes). It
// renders in InstructionsModeAuto; loadInstructionsMode selects another mode.
func loadInstructions(workDir string, maxBytes int) (content, path string, err error) {
	return loadInstructionsMode(workDir, maxBytes, InstructionsModeAuto)
}

// loadInstructionsMode is loadInstructions with an explicit render mode.
func loadInstructionsMode(workDir string, maxBytes int, mode InstructionsMode) (content, path string, err error) {
	dir := workDir
	for {
		if p, data, found := readInstructionFile(dir); found {
			body, err := validateInstructions(p, data, maxBytes, mode)
			if err != nil {
				return "", "", err
			}
			return body, displayPath(workDir, p), nil
		}
		// Stop once we've checked the git root itself.
		if isDir(filepath.Join(dir, ".git")) {
			return "", "", nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", nil // filesystem root
		}
		dir = parent
	}
}

// readInstructionFile returns the first readable instruction file in dir, by
// the preference order in instructionsFilenames. os.ReadFile follows symlinks;
// an unreadable file or directory of the same name is skipped.
func readInstructionFile(dir string) (path string, data []byte, found bool) {
	for _, name := range instructionsFilenames {
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		return p, b, true
	}
	return "", nil, false
}

// validateInstructions returns the segment body for a read instruction file,
// applying the size cap. Because the agents.md format is schema-less, the only
// "malformed" states are encoding-level: a present-but-unusable file (invalid
// UTF-8, or empty/whitespace-only) is a hard error — the project meant to
// supply instructions and the agent must not silently run without them. Size
// is not malformedness: an oversize file is truncated, not rejected.
func validateInstructions(path string, data []byte, maxBytes int, mode InstructionsMode) (string, error) {
	if !utf8.Valid(data) {
		return "", fmt.Errorf("engine: instructions file %s is not valid UTF-8", path)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("engine: instructions file %s is empty", path)
	}
	return renderInstructions(path, data, maxBytes, mode), nil
}

// truncateInstructions applies the byte cap to an already-validated
// instruction file. It is LOUD on both channels: the model reads the in-band
// marker formatTruncationMarker builds, and the operator reads one WARN log
// line with the original and the kept byte counts. Neither channel existed
// before: a 408 KiB AGENTS.md was cut to 64 KiB in silence, and no reader —
// model or operator — could tell the file was incomplete.
//
// A negative maxBytes disables the cap. A file at or under the cap is
// returned verbatim, with no marker and no log line. The instruction file is
// read once per session (ensureInstructions caches the segment), so an
// oversize file writes one WARN line per session, never one per request.
func truncateInstructions(path string, data []byte, maxBytes int) string {
	return truncateInstructionsOf(path, data, maxBytes, len(data))
}

// truncateInstructionsOf is truncateInstructions over a PREFIX of a larger
// file: total is the whole file's byte size, which the marker and the log
// line report. The outline path truncates the first section alone
// (renderInstructions), and a marker that reported that slice's size would
// tell the model the file is far smaller than it is.
func truncateInstructionsOf(path string, data []byte, maxBytes, total int) string {
	if maxBytes < 0 || len(data) <= maxBytes {
		return string(data)
	}
	// Trim any trailing partial rune so the truncated body stays valid
	// UTF-8 (the full data is already known valid by the caller).
	capped := data[:maxBytes]
	for len(capped) > 0 && !utf8.Valid(capped) {
		capped = capped[:len(capped)-1]
	}
	slog.Warn("engine: instructions file truncated",
		"path", path,
		"original_bytes", total,
		"kept_bytes", len(capped),
		"dropped_bytes", total-len(capped),
		"limit_bytes", maxBytes,
	)
	return string(capped) + "\n" + formatTruncationMarker(path, total, len(capped))
}

// isDir reports whether path is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// displayPath renders p relative to workDir when possible, else returns p
// unchanged, so the injected segment names a short, readable location.
func displayPath(workDir, p string) string {
	if rel, err := filepath.Rel(workDir, p); err == nil {
		return rel
	}
	return p
}

// formatInstructions builds the system-prompt segment for an instruction
// file.
func formatInstructions(path, content string) string {
	return fmt.Sprintf("Project instructions from %s:\n\n%s", path, content)
}

// ensureInstructions loads and caches the instruction segment on first call,
// returning any error from a present-but-unusable file. It is a no-op on later
// calls (the cached error, if any, is returned again), so the file is read at
// most once per session even though the segment is appended to every request's
// system prompt.
func (s *Session) ensureInstructions() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instrLoaded {
		return s.instrErr
	}
	s.instrLoaded = true
	seg, err := s.buildInstructionSegment()
	if err != nil {
		s.instrErr = err
		return err
	}
	s.instrSeg = seg
	return nil
}

// buildInstructionSegment resolves the configured instruction source and
// returns the formatted segment, or "" when injection is disabled or no file
// is found. A present-but-unusable file returns an error. Caller holds s.mu.
func (s *Session) buildInstructionSegment() (string, error) {
	ic := s.cfg.Instructions
	if ic != nil && ic.Disabled {
		return "", nil
	}
	maxBytes := resolveInstructionsMaxBytes(ic)
	mode := InstructionsModeAuto
	if ic != nil {
		mode = ic.Mode
	}
	if ic != nil && ic.Path != "" {
		// A relative override resolves against the session's WorkDir, not
		// the process cwd — embedders may set WorkDir independently.
		path := ic.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.cfg.WorkDir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil // missing/unreadable override: no segment, no error
		}
		body, err := validateInstructions(path, data, maxBytes, mode)
		if err != nil {
			return "", err
		}
		s.instrPath = ic.Path
		return formatInstructions(ic.Path, body), nil
	}
	content, path, err := loadInstructionsMode(s.cfg.WorkDir, maxBytes, mode)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	s.instrPath = path
	return formatInstructions(path, content), nil
}

// instructionSegment returns the cached instruction segment (possibly empty).
func (s *Session) instructionSegment() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instrSeg
}
