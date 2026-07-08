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
// Discovery touches disk, so it happens lazily on the first Prompt of a
// session (never at NewSession — the startup budget rule) and the result is
// cached for the session's life. Instructions are never written to the session
// log: the log stores only canonical messages, and instructions are re-read
// fresh whenever a session is loaded and prompted again.

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// instructionsFilenames are the project-instruction file names checked in each
// directory, in preference order (AGENTS.md wins over the singular AGENT.md).
var instructionsFilenames = []string{"AGENTS.md", "AGENT.md"}

// maxInstructionsBytes caps how much of an instruction file is read into the
// system prompt. Content beyond the cap is dropped and replaced with
// truncationMarker.
const maxInstructionsBytes = 64 * 1024

// truncationMarker is appended when an instruction file exceeds the cap.
const truncationMarker = "[... truncated: AGENTS.md exceeds 64 KiB ...]"

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
}

// loadInstructions searches from workDir upward for AGENTS.md (falling back to
// AGENT.md) and returns its (possibly truncated) content plus a display path
// relative to workDir. The walk stops at the first directory containing a .git
// entry — that directory is checked for an instructions file before stopping —
// or at the filesystem root. A missing file yields empty strings and no error.
func loadInstructions(workDir string) (content, path string, err error) {
	dir := workDir
	for {
		if p, data, found := readInstructionFile(dir); found {
			body, err := validateInstructions(p, data)
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
func validateInstructions(path string, data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", fmt.Errorf("engine: instructions file %s is not valid UTF-8", path)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("engine: instructions file %s is empty", path)
	}
	if len(data) > maxInstructionsBytes {
		// Trim any trailing partial rune so the truncated body stays valid
		// UTF-8 (the full data is already known valid above).
		capped := data[:maxInstructionsBytes]
		for len(capped) > 0 && !utf8.Valid(capped) {
			capped = capped[:len(capped)-1]
		}
		return string(capped) + "\n" + truncationMarker, nil
	}
	return string(data), nil
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
	if ic != nil && ic.Path != "" {
		data, err := os.ReadFile(ic.Path)
		if err != nil {
			return "", nil // missing/unreadable override: no segment, no error
		}
		body, err := validateInstructions(ic.Path, data)
		if err != nil {
			return "", err
		}
		return formatInstructions(ic.Path, body), nil
	}
	content, path, err := loadInstructions(s.cfg.WorkDir)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	return formatInstructions(path, content), nil
}

// instructionSegment returns the cached instruction segment (possibly empty).
func (s *Session) instructionSegment() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instrSeg
}
