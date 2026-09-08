// Project-instruction injection: an AGENTS.md discovered near the working
// directory is appended to the system prompt so repo-specific guidance
// applies without the user having to ask for it.
//
// Format and discovery follow the agents.md convention
// (https://agents.md/): the file is schema-less standard Markdown — the agent
// simply parses the text, using no fixed headings — and a nested file wins a
// conflict against an ancestor. loadInstructionChain implements that: it finds
// the repository root — the nearest ancestor of WorkDir with a .git entry
// (file or directory, so a worktree or submodule checkout still resolves the
// right root), or WorkDir itself when no ancestor holds one — and injects
// every AGENTS.md (or AGENT.md fallback) found from that root down to WorkDir
// inclusive, root first. os.ReadFile follows symlinks, so the spec's
// `ln -s AGENTS.md AGENT.md` compatibility setup works transparently.
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
	// MaxBytes caps the instruction bytes injected PER FILE. Zero (the zero
	// value) takes defaultMaxInstructionsBytes (64 KiB); a positive value sets
	// the per-file cap; a NEGATIVE value disables both this cap and the chain
	// cap below, so every file is injected however large it is. Truncation is
	// always loud — see truncateInstructions.
	//
	// A WorkDir several directories below the repository root can inject
	// several files (see loadInstructionChain), so capChainTotal makes a
	// BEST-EFFORT second pass at chainCeilingMultiplier * MaxBytes: over that
	// total, it drops middle files — never the root, which carries the
	// routing table, and never the deepest, which names WorkDir's own rules —
	// until the chain fits or no middle file remains. The root and the
	// deepest file are never capped or dropped, so an oversize outline
	// rendering (engine/instructions_outline.go) on either one can still push
	// the actual total past this ceiling; it bounds the MIDDLE, not a hard
	// maximum on the whole chain. See capChainTotal.
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

// hasGitEntry reports whether dir holds a .git entry, marking it a repository
// root. A normal checkout uses a directory; a git worktree or a submodule
// checkout uses a regular FILE holding "gitdir: ...". Either one bounds the
// upward walk — an isDir-only check treats a worktree's .git file as absent,
// so the walk climbs past the worktree root into whatever lies above it (a
// sibling worktree, the main checkout, or an unrelated tree outside the repo
// entirely) and injects that ancestor's AGENTS.md as if it were the root's.
func hasGitEntry(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
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

// instructionFile is one AGENTS.md/AGENT.md found on the path from the repo
// root down to WorkDir, with its (possibly truncated) rendered body.
type instructionFile struct {
	path string // display path, per displayPath
	body string
}

// loadInstructionChain finds the repository root — the nearest ancestor of
// workDir with a .git entry (file or directory; see hasGitEntry), or, when
// WorkDir is not inside a repository, workDir itself — and returns every
// AGENTS.md/AGENT.md found from that root down to workDir inclusive, root
// first. A directory with neither file contributes nothing; the chain is
// empty, with a nil error, when no directory on the path holds one. maxBytes
// and mode apply per file; capChainTotal then makes a best-effort second pass
// to trim middle files toward chainCeilingMultiplier*maxBytes (see
// capChainTotal's own doc for what that pass does and does not bound).
//
// Without a repository boundary, only workDir's own file counts: a session
// whose WorkDir sits under an arbitrary, non-repository directory (a scratch
// folder, $HOME) must not inject an ancestor there as if it were a repository
// root. Every ENGINE test that sets WorkDir to a fresh t.TempDir() with no
// .git relies on this too — without it, the walk would climb to the
// filesystem root and could pick up a stray AGENTS.md the test never wrote
// (a developer machine's $HOME, or a box image file above /tmp).
//
// A malformed file (invalid UTF-8, or empty/whitespace-only) found in the
// directory NEAREST workDir — the file loadInstructionChain's predecessor,
// the single-closest-file walk, would have found and failed on — still fails
// the whole load, matching that walk's existing contract: a project that
// meant to supply instructions must not run silently without them. A
// malformed file found in any OTHER (more ancestral) directory is skipped
// with a logged warning naming its path instead: an unrelated ancestor's
// broken file must not fail every session rooted below it.
func loadInstructionChain(workDir string, maxBytes int, mode InstructionsMode) ([]instructionFile, error) {
	type found struct {
		dir  string
		path string
		data []byte
	}
	var chain []found // workDir-first: chain[0], if present, is the nearest file
	repoRoot := false
	for dir := workDir; ; {
		if p, data, ok := readInstructionFile(dir); ok {
			chain = append(chain, found{dir: dir, path: p, data: data})
		}
		if hasGitEntry(dir) {
			repoRoot = true
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root: no repository boundary found
		}
		dir = parent
	}
	if !repoRoot {
		if len(chain) > 0 && chain[0].dir == workDir {
			chain = chain[:1]
		} else {
			chain = nil
		}
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i] // root first
	}
	var files []instructionFile
	for i, f := range chain {
		body, err := validateInstructions(f.path, f.data, maxBytes, mode)
		if err != nil {
			if i == len(chain)-1 { // the nearest file, now last after the reversal
				return nil, err
			}
			slog.Warn("engine: instructions file skipped, malformed", "path", f.path, "err", err)
			continue
		}
		files = append(files, instructionFile{path: displayPath(workDir, f.path), body: body})
	}
	return capChainTotal(files, maxBytes), nil
}

// chainCeilingMultiplier targets the CHAIN total against runaway monorepo
// depth: maxBytes caps one file, chainCeilingMultiplier*maxBytes is the
// target capChainTotal trims MIDDLE files toward. See capChainTotal for what
// this target does and does not guarantee.
const chainCeilingMultiplier = 4

// capChainTotal makes a BEST-EFFORT pass at the chain ceiling
// (chainCeilingMultiplier*maxBytes): it trims files strictly between the
// root (files[0]) and the deepest file (files[len(files)-1]) one at a time,
// nearest the root first, until the total fits under that target or no
// middle file remains. The root and the deepest file are never trimmed or
// dropped — the root always carries the routing table naming every scoped
// file, and the deepest always names WorkDir's own rules — so this is a
// bound on the MIDDLE of the chain, not a hard ceiling on its total: an
// oversize outline rendering (engine/instructions_outline.go) on the root or
// the deepest file, which this pass never touches, can still push the
// actual total past chainCeilingMultiplier*maxBytes. A negative maxBytes
// disables the per-file cap and, with it, this pass (an operator who asked
// for the whole file gets the whole chain too).
func capChainTotal(files []instructionFile, maxBytes int) []instructionFile {
	if maxBytes < 0 || len(files) <= 2 {
		return files
	}
	ceiling := maxBytes * chainCeilingMultiplier
	total := 0
	for _, f := range files {
		total += len(f.body)
	}
	if total <= ceiling {
		return files
	}
	kept := append([]instructionFile(nil), files...)
	var dropped []string
	for i := 1; i < len(kept)-1 && total > ceiling; {
		total -= len(kept[i].body)
		dropped = append(dropped, kept[i].path)
		kept = append(kept[:i], kept[i+1:]...)
	}
	slog.Warn("engine: instructions chain truncated to fit the chain byte ceiling",
		"ceiling_bytes", ceiling,
		"dropped_files", strings.Join(dropped, ", "),
	)
	return kept
}

// formatInstructions builds the system-prompt segment for the discovered
// instruction files, root to working directory. A single file keeps the
// plain header a session with only one AGENTS.md has always seen; more than
// one file adds a precedence line, since a nested file can now disagree with
// an ancestor's.
func formatInstructions(files []instructionFile) string {
	if len(files) == 1 {
		return fmt.Sprintf("Project instructions from %s:\n\n%s", files[0].path, files[0].body)
	}
	var b strings.Builder
	b.WriteString("Project instructions, root to working directory. The deepest file wins on conflict.\n")
	for _, f := range files {
		b.WriteString("\nFrom " + f.path + ":\n\n" + f.body + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
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
		return formatInstructions([]instructionFile{{path: ic.Path, body: body}}), nil
	}
	files, err := loadInstructionChain(s.cfg.WorkDir, maxBytes, mode)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", nil
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.path
	}
	s.instrPath = strings.Join(paths, ", ")
	return formatInstructions(files), nil
}

// instructionSegment returns the cached instruction segment (possibly empty).
func (s *Session) instructionSegment() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instrSeg
}
