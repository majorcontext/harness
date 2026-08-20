// Tool-result handles: retention of an oversized TEXT tool result into a
// per-session sidecar store, replaced in canonical history by a short
// preview carrying a session-monotonic handle (trh_N) the model reads back
// with the read_tool_result tool. See
// docs/plans/2026-08-19-tool-result-handles.md.
//
// # Where this sits
//
// Retention runs in runToolCalls (engine.go), on the parts a tool just
// produced, strictly BEFORE the ToolResult is wrapped into a message and
// handed to Session.append. That placement is the whole reason this feature
// needs no transcoder change and no wire-normalization change: by the time
// message.NormalizeForWire or any provider adapter sees the message, it is
// an ordinary ToolResult carrying ordinary Text parts. Handles are a fact
// about how the engine BUILT the result, never a new part kind, never a new
// wire shape.
//
// It is also why retention does not violate AGENTS.md's additive-only
// live-repair rule. That rule governs a repair that runs over live or
// persisted history (ResolveOrphanToolCalls). Retention is not a repair: it
// runs once, on a message that does not yet exist in history, at the one
// point the engine already decides what the ToolResult's content is.
//
// # Text only
//
// Only *message.Text parts are ever retained. A message.Blob (read_file's
// image path, MCP's mcpContentToParts) passes through untouched and in
// order, after the preview parts. Image bytes are already bounded by
// imageclamp.Clamp at transcode time; retaining them here would duplicate
// that bound in a layer that cannot see the provider's limit.
package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/majorcontext/harness/message"
)

const (
	// toolResultHandlePrefix prefixes every minted handle. Handles are
	// "trh_<N>" with N session-monotonic from 1 — deliberately NOT typeid
	// (see id.go): a handle is a short, model-typed token that has to
	// survive being read off a preview line and typed back into a tool
	// call, so it is optimized for a model retyping it correctly, not for
	// global uniqueness. Scope is one session; the sidecar directory is
	// per session too, so a collision across sessions is impossible.
	toolResultHandlePrefix = "trh_"

	// toolResultsDirName is the sidecar root under Config.SessionDir. The
	// retained bytes deliberately do NOT live in the session's own .jsonl
	// log: that log is fully replayed by LoadSession on every resume, so
	// inlining megabytes there would make resume pay for exactly the bytes
	// retention exists to keep out of memory.
	toolResultsDirName = "toolresults"
)

// defaultToolResultInlineBytes / defaultToolResultRetainedBytes are the
// PRODUCT defaults, supplied by the config/CLI layer
// (config.ToolResultInlineBytesValue), never by engine.Config's zero value.
// That split is the same one Config.PromptRetries uses and it is
// load-bearing: an embedder building a bare engine.Config gets today's
// behavior byte for byte, with no sidecar directory ever created.
const (
	defaultToolResultInlineBytes   = 16384
	defaultToolResultRetainedBytes = 4 * 1024 * 1024
)

// toolResultMeta is one retained result's metadata, held in memory for the
// life of the session and rebuilt on resume from the durable
// toolresult.retained records (see store.go's LoadSession fold). It carries
// no content — the bytes live in the sidecar file named by Path.
type toolResultMeta struct {
	Handle string
	Tool   string
	Bytes  int
	Lines  int
}

// toolResultPreviewHeader renders the EXACT preview header line documented
// in docs/plans/2026-08-19-tool-result-handles.md §2.1. It exists as one
// function with one caller precisely so the documented format and the
// produced format cannot drift: TestToolResultPreviewHeaderExactFormat pins
// this string literally, character for character, rather than reassembling
// it from the same fmt verbs (which would pass no matter what the format
// became).
//
// The trailing clause names the handle inside a literal call shape because
// it is the only in-band documentation the model gets at the moment it
// needs it — a model that sees this line has, by construction, just lost
// access to the bytes it was expecting.
func toolResultPreviewHeader(handle, tool string, totalBytes, totalLines, previewBytes int) string {
	return fmt.Sprintf(
		"[tool result retained: handle=%s tool=%s bytes=%d lines=%d preview_bytes=%d — read the rest with read_tool_result(handle=%q)]",
		handle, tool, totalBytes, totalLines, previewBytes, handle,
	)
}

// toolResultCapHeader renders the header used when retention is REFUSED
// because the per-session retained-bytes ceiling
// (Config.ToolResultRetainedBytes) is already reached. It deliberately
// carries no handle: nothing was written, so there is nothing to read back,
// and emitting a handle-shaped token for bytes that do not exist would be
// worse than saying plainly that they are gone.
func toolResultCapHeader(tool string, totalBytes, previewBytes int) string {
	return fmt.Sprintf(
		"[tool result truncated: tool=%s bytes=%d preview_bytes=%d — retention cap reached for this session, the remainder is discarded]",
		tool, totalBytes, previewBytes,
	)
}

// toolResultInlineLimit resolves the effective inline limit. A value <= 0
// DISABLES retention entirely (the documented contract), and so does an
// unset Config.SessionDir: without a session directory there is nowhere to
// durably put the bytes, and a preview naming a handle that can never be
// read is strictly worse than no preview at all.
func (s *Session) toolResultInlineLimit() int {
	if s.cfg.SessionDir == "" {
		return 0
	}
	return s.cfg.ToolResultInlineBytes
}

// toolResultRetainedLimit resolves the effective per-session retained-bytes
// ceiling. <= 0 disables the ceiling (unbounded retention), matching the
// documented key contract.
func (s *Session) toolResultRetainedLimit() int {
	return s.cfg.ToolResultRetainedBytes
}

// toolResultsDir is this session's sidecar directory. Per session, so a
// handle minted in one session can never name another session's bytes.
func (s *Session) toolResultsDir() string {
	return filepath.Join(s.cfg.SessionDir, toolResultsDirName, s.ID)
}

// toolResultPath is the sidecar file for one handle. One flat file per
// handle holding the joined original text verbatim: no framing, no
// compression, no JSON, so read_tool_result can range-read it with a
// bufio.Scanner without ever loading the whole file.
func (s *Session) toolResultPath(handle string) string {
	return filepath.Join(s.toolResultsDir(), handle+".txt")
}

// parseToolResultHandle validates a handle token and returns its numeric
// part. Used both by the tool (rejecting a malformed argument before any
// filesystem path is built from it — the same defense-in-depth posture
// ValidSessionID takes for a session id) and by LoadSession's replay fold
// (skipping a malformed durable record rather than trusting it).
func parseToolResultHandle(h string) (int64, bool) {
	rest, ok := strings.CutPrefix(h, toolResultHandlePrefix)
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// countLines counts the lines in s: one per '\n', plus one for a non-empty
// final line with no trailing newline. Empty text is zero lines.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// splitToolResultParts separates content into the joined text of every
// *message.Text part and every OTHER part, in original order. The joined
// text is what retention measures and (possibly) retains; the other parts
// are what must pass through untouched.
func splitToolResultParts(content message.Parts) (text string, others message.Parts) {
	var b strings.Builder
	for _, p := range content {
		if t, ok := p.(*message.Text); ok {
			b.WriteString(t.Text)
			continue
		}
		others = append(others, p)
	}
	return b.String(), others
}

// truncateUTF8 returns the longest prefix of s that is at most n bytes and
// does not split a multi-byte rune. Retention's preview is a byte budget,
// but a preview cut mid-rune would put invalid UTF-8 into the canonical
// message — and from there into a provider request, where at least one
// adapter's JSON encoding would silently replace it with U+FFFD.
func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// Walk back off any continuation bytes (0b10xxxxxx) at the cut point.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// maybeRetainToolResult is the engine's single retention entry point, called
// from runToolCalls with the parts a tool just produced. It returns the
// parts that should actually become the ToolResult's Content.
//
// It is a total no-op — same slice, nothing written, no directory created —
// when retention is disabled (limit <= 0 or no SessionDir) or when the
// joined text is within the limit. That no-op path is the one every existing
// test in this package travels, which is why none of them needed changing.
//
// Failure to write the sidecar file is NOT fatal to the tool call: retention
// is an optimization, and a tool result the model can read is always better
// than an error. On a write failure the original parts are returned intact
// (the pre-retention behavior), and the error is recorded in lastPersistErr
// exactly like every other best-effort persist path in this package.
func (s *Session) maybeRetainToolResult(tool string, content message.Parts) message.Parts {
	limit := s.toolResultInlineLimit()
	if limit <= 0 {
		return content
	}
	text, others := splitToolResultParts(content)
	if len(text) <= limit {
		return content
	}

	preview := truncateUTF8(text, limit)

	// Per-session ceiling check. Refusing is a real outcome, not an error:
	// the model still gets the preview, and the cap header says plainly
	// that the remainder is gone rather than handing out a handle for
	// bytes that were never written.
	if cap := s.toolResultRetainedLimit(); cap > 0 {
		s.mu.Lock()
		used := s.toolResultBytes
		s.mu.Unlock()
		if used+len(text) > cap {
			return append(message.Parts{
				&message.Text{Text: toolResultCapHeader(tool, len(text), len(preview))},
				&message.Text{Text: preview},
			}, others...)
		}
	}

	handle, err := s.writeRetainedToolResult(tool, text)
	if err != nil {
		s.mu.Lock()
		s.lastPersistErr = err
		s.mu.Unlock()
		return content
	}

	header := toolResultPreviewHeader(handle, tool, len(text), countLines(text), len(preview))
	return append(message.Parts{
		&message.Text{Text: header},
		&message.Text{Text: preview},
	}, others...)
}

// writeRetainedToolResult mints the next handle, writes the sidecar file,
// records the durable toolresult.retained pointer record, and registers the
// handle's metadata in memory. It returns the minted handle.
//
// Ordering is file-then-record, deliberately. A crash between the two
// degrades to an orphaned file on disk plus a handle the session never knew
// about: wasted bytes, never wrong bytes. The reverse order would let a
// resumed session hand out a handle whose file was never written, which is
// the strictly worse failure — it turns a crash into a broken read every
// later turn.
//
// The handle ID is minted (and BURNED) before the write is attempted, so a
// failed write never lets a later retention reuse the same number: reuse
// would fold two different results under one handle on replay, exactly the
// hazard EnqueuePromptDurable's burned-ID rule guards against for queue IDs.
func (s *Session) writeRetainedToolResult(tool, text string) (string, error) {
	s.mu.Lock()
	n := s.toolResultNextID
	s.toolResultNextID++
	s.mu.Unlock()

	handle := toolResultHandlePrefix + strconv.FormatInt(n, 10)

	dir := s.toolResultsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("engine: tool-result retention: %w", err)
	}
	if err := os.WriteFile(s.toolResultPath(handle), []byte(text), 0o644); err != nil {
		return "", fmt.Errorf("engine: tool-result retention: %w", err)
	}

	meta := toolResultMeta{Handle: handle, Tool: tool, Bytes: len(text), Lines: countLines(text)}

	s.mu.Lock()
	if s.toolResults == nil {
		s.toolResults = make(map[string]toolResultMeta)
	}
	s.toolResults[handle] = meta
	s.toolResultBytes += len(text)
	s.persistToolResultRetainedLocked(meta)
	s.mu.Unlock()

	return handle, nil
}

// lookupToolResult returns one handle's metadata. Safe for concurrent use.
func (s *Session) lookupToolResult(handle string) (toolResultMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.toolResults[handle]
	return m, ok
}

// knownToolResultHandles lists this session's handles in mint order, most
// recent last, bounded to the newest max entries. It exists for the
// unknown-handle error path: telling a model which handles DO exist is what
// turns a dead end into a recoverable mistake, and the bound is what stops
// a long session's error message from becoming its own context problem.
func (s *Session) knownToolResultHandles(max int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var nums []int64
	for h := range s.toolResults {
		if n, ok := parseToolResultHandle(h); ok {
			nums = append(nums, n)
		}
	}
	sortInt64s(nums)
	if max > 0 && len(nums) > max {
		nums = nums[len(nums)-max:]
	}
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		out = append(out, toolResultHandlePrefix+strconv.FormatInt(n, 10))
	}
	return out
}

// sortInt64s sorts in place, ascending. A tiny insertion sort rather than a
// sort.Slice call: the input is a handle list, in practice single digits to
// low tens, and this keeps the package's import surface unchanged.
func sortInt64s(a []int64) {
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}

// errToolResultFileMissing marks the "handle is known, sidecar file is
// gone" case (an operator wiped the directory, a volume rolled back) so
// readRetainedToolResult's caller can report it in fixed, safe wording
// rather than surfacing a raw *os.PathError carrying an absolute
// filesystem path into model-visible tool output.
var errToolResultFileMissing = errors.New("retained tool result file is missing")

// openRetainedToolResult opens one handle's sidecar file, mapping a
// not-exist error to errToolResultFileMissing.
func (s *Session) openRetainedToolResult(handle string) (*os.File, error) {
	f, err := os.Open(s.toolResultPath(handle))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errToolResultFileMissing
	}
	if err != nil {
		return nil, errToolResultFileMissing
	}
	return f, nil
}
