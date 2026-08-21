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

// toolResultMeta is one retained result's metadata, held in memory for the
// life of the session and rebuilt on resume from the durable
// toolresult.retained records (see store.go's LoadSession fold). It carries
// no content — the bytes live in the sidecar file named by Path.
type toolResultMeta struct {
	Handle string
	Tool   string
	Bytes  int
	Lines  int
	// Head is the first toolResultIndexHeadBytes bytes of the MASKED,
	// on-disk text (see maybeRetainToolResult/writeRetainedToolResult),
	// UTF-8-safely truncated. It exists solely so compaction's
	// retained-results index (compact.go) can name a handle recognizably
	// without re-opening its sidecar file — see F3.
	Head string
}

// toolResultIndexHeadBytes bounds toolResultMeta.Head — "first ~80 chars"
// per review finding F3(a). A byte bound, not a rune count, for the same
// reason truncateUTF8 exists: this must never split a multi-byte rune.
const toolResultIndexHeadBytes = 80

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
// because retaining THIS result would push the per-session retained-bytes
// total (Config.ToolResultRetainedBytes) over the ceiling. It deliberately
// carries no handle: nothing was written, so there is nothing to read back,
// and emitting a handle-shaped token for bytes that do not exist would be
// worse than saying plainly that they are gone.
//
// A round-3 review finding caught an earlier version of this wording
// overstating permanence in BOTH directions: it claimed "the cap has been
// reached" (implying accumulation from real retentions, when a single
// oversized result on a FRESH session — used=0 — refuses just as
// unconditionally) and "no further tool result will be retained this
// session" (false: a refusal never increments toolResultBytes — only
// writeRetainedToolResult does, and that never runs on this path — so a
// LATER, SMALLER result can still fit under the same ceiling and succeed).
// TestToolResultCapHeaderDoesNotOverstatePermanence drives that exact
// contradiction end to end. The wording now says only what is always true:
// retaining THIS result would exceed the remaining budget, and THIS
// result's remainder is gone for good — with no claim about what happens
// next.
func toolResultCapHeader(tool string, totalBytes, previewBytes int) string {
	return fmt.Sprintf(
		"[tool result truncated: tool=%s bytes=%d preview_bytes=%d — retaining this result would exceed the per-session retention budget; its remainder is discarded irrecoverably, though a smaller result later this session may still be retained]",
		tool, totalBytes, previewBytes,
	)
}

// toolResultInlineLimit resolves the effective inline limit. A value <= 0
// DISABLES retention entirely (the documented contract), and so does an
// unset Config.SessionDir: without a session directory there is nowhere to
// durably put the bytes, and a preview naming a handle that can never be
// read is strictly worse than no preview at all.
//
// This reads Config.ToolResultInlineBytes as given, with no default
// substitution — a bare engine.Config's zero value (0) means "disabled",
// byte for byte, never a silent 16384. The PRODUCT default lives one layer
// up, in config.Config.ToolResultInlineBytesValue (config/config.go) —
// the sole authoritative source; round-5 review removed a duplicate,
// unreferenced copy of that number that had drifted into this file.
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
//
// The numeric part must be DIGITS ONLY, no leading zero, no sign: the write
// path (strconv.FormatInt) never produces "trh_+1" or "trh_01", so
// strconv.ParseInt's looser grammar (which accepts both as spellings of 1)
// would let those parse as valid handles that are really just alternate,
// non-canonical spellings of "trh_1" — a false-negative-shaped bug review
// finding F13 flagged. A caller-supplied handle passes here only if it is
// byte-for-byte a string writeRetainedToolResult could have minted.
func parseToolResultHandle(h string) (int64, bool) {
	rest, ok := strings.CutPrefix(h, toolResultHandlePrefix)
	if !ok || rest == "" {
		return 0, false
	}
	if rest[0] < '1' || rest[0] > '9' {
		return 0, false
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return 0, false
		}
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
	// read_tool_result's OWN output must never be re-retained. Without this
	// exemption, a read whose result exceeds the inline limit (trivially
	// true whenever max_bytes is set above the limit — the tool's own
	// documented default and cap both routinely are) mints a NEW handle
	// instead of returning inline, making the documented max_bytes ceiling
	// unreachable and doubling the on-disk bytes for content that is
	// already durably retained under its source handle (review finding
	// F2). read_tool_result output IS the recovery path FOR retention; it
	// must never re-enter it.
	if tool == readToolResultToolName {
		return content
	}
	limit := s.toolResultInlineLimit()
	if limit <= 0 {
		return content
	}
	text, others := splitToolResultParts(content)
	if len(text) <= limit {
		// Fast path: skip masking entirely when the ORIGINAL is already
		// within budget. Masking can only make this MORE true (see below),
		// never less — this is a pure perf optimization (the overwhelming
		// majority of tool calls never approach the inline limit at all),
		// not a scope decision: results that stay inline are not masked
		// today regardless of this fast path (masking protects what gets
		// RETAINED — see toolresult_secrets.go's package doc comment for
		// the documented scope).
		return content
	}

	// Mask exactly ONCE, up front — review findings N2/N5. Everything
	// downstream (the preview that goes inline to the model, the bytes
	// written to disk, and the accounting against the retention ceiling)
	// must agree on the SAME masked text. The first cut of this feature
	// masked only what writeRetainedToolResult wrote to disk: the PREVIEW
	// — which goes straight into the provider request, unlike the sidecar
	// file — was built from the unmasked original, so a secret sitting in
	// the first `limit` bytes reached the model in cleartext regardless of
	// masking existing at all. meta.Bytes/Lines are derived from `masked`
	// too (not the original `text`), because that is what is actually ON
	// DISK: a header or read_tool_result advertising the ORIGINAL length
	// was pointing at a size the file did not have (N2).
	masked := maskSecrets(text)

	// Round-5 review finding: the RETENTION DECISION must be measured
	// against the masked length too, not just the header/disk/ceiling
	// accounting above it. `text` exceeding `limit` only means retention
	// is being CONSIDERED — masking can shrink it well under `limit` (a
	// long secret value collapses to "***"), and gating on the pre-mask
	// size burned a handle and wrote a sidecar file for a result that fit
	// inline all along once masked, with a "read the rest" header pointing
	// at nothing left to read. If masking alone already brought it within
	// budget, return the masked text inline — no handle, no sidecar file.
	if len(masked) <= limit {
		return append(message.Parts{&message.Text{Text: masked}}, others...)
	}

	preview := truncateUTF8(masked, limit)

	// Per-session ceiling check. Refusing is a real outcome, not an error:
	// the model still gets the preview, and the cap header says plainly
	// that the remainder is gone rather than handing out a handle for
	// bytes that were never written. Measured against len(masked) — the
	// ceiling counts what actually lands on disk, same as Bytes above.
	if cap := s.toolResultRetainedLimit(); cap > 0 {
		s.mu.Lock()
		used := s.toolResultBytes
		s.mu.Unlock()
		if used+len(masked) > cap {
			return append(message.Parts{
				&message.Text{Text: toolResultCapHeader(tool, len(masked), len(preview))},
				&message.Text{Text: preview},
			}, others...)
		}
	}

	handle, err := s.writeRetainedToolResult(tool, masked)
	if err != nil {
		s.mu.Lock()
		s.lastPersistErr = err
		s.mu.Unlock()
		return content
	}

	header := toolResultPreviewHeader(handle, tool, len(masked), countLines(masked), len(preview))
	return append(message.Parts{
		&message.Text{Text: header},
		&message.Text{Text: preview},
	}, others...)
}

// writeRetainedToolResult mints the next handle, writes the sidecar file,
// records the durable toolresult.retained pointer record, and registers the
// handle's metadata in memory. It returns the minted handle.
//
// text is written to disk EXACTLY as given — this function does not mask
// it. maybeRetainToolResult (the only real production caller) masks once,
// up front, and passes the already-masked text down here, so the preview
// that goes inline to the model and the bytes that land on disk are always
// masked in agreement (review findings N2/N5; see maybeRetainToolResult's
// doc comment). A direct test caller that passes raw, unmasked text is
// exercising the write/persist/resume mechanics only, not masking — that is
// covered separately by the maybeRetainToolResult-path masking tests.
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
	// 0o700/0o600, not 0o755/0o644: a retained result is arbitrary tool
	// output, routinely including secrets a command printed (an env dump, a
	// leaked credential in a log line) — see review finding F4. Neither the
	// directory nor the file should be group- or world-readable.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("engine: tool-result retention: %w", err)
	}
	if err := os.WriteFile(s.toolResultPath(handle), []byte(text), 0o600); err != nil {
		return "", fmt.Errorf("engine: tool-result retention: %w", err)
	}

	meta := toolResultMeta{
		Handle: handle,
		Tool:   tool,
		// Bytes/Lines are measured from `text` AS WRITTEN — the on-disk,
		// post-mask length (review finding N2). The caller already masked
		// before reaching here, so this is simply "what's on disk," not a
		// second masking decision.
		Bytes: len(text),
		Lines: countLines(text),
		Head:  truncateUTF8(text, toolResultIndexHeadBytes),
	}

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

// retainedResultsIndexMaxHandles bounds how many handles
// retainedResultsIndexPart lists individually (review finding N8):
// unbounded, 200 handles measured at roughly 6.9k tokens of summary — with
// the retention ceiling disabled (Config.ToolResultRetainedBytes <= 0) a
// long session can mint arbitrarily many, and an index that size defeats
// the point of compaction, which is to REDUCE what the next request pays
// for. Only the newest N are listed individually; older ones are named by
// count only ("...and M older retained results (not listed; still on disk,
// use read_tool_result with the handle number if known)").
const retainedResultsIndexMaxHandles = 32

// retainedResultsIndexPart builds compaction's deterministic, machine-
// written index of the newest still-live tool-result handles (review
// finding F3(a)) — one line per handle, sorted ascending by number, naming
// the handle, its source tool, its byte size, and a short head excerpt so a
// model skimming the summary can recognize what it is without opening it.
// Returns nil when there are no handles at all, so an ordinary compaction
// with nothing retained gains no spurious empty block.
//
// Deliberately NOT derived from the LLM's summary text: compactionSystemPrompt
// forbids the summarizer from transcribing tool output, so it cannot be
// trusted to carry handles forward on its own, and a prompt-dependent
// mechanism for something this load-bearing (an orphaned handle stays
// invisible AND still counted against the monotonic retention ceiling for
// the rest of the session) is exactly the failure mode this guards against.
// This index covers every handle the session currently knows about, not
// only handles minted within the folded range — a handle minted long before
// the fold is exactly as reachable-only-through-a-preview-line as one
// minted inside it, and exactly as easy to lose track of once that line is
// gone. It is bounded to the newest retainedResultsIndexMaxHandles (N8) —
// the newest, because those are the ones most likely to still matter to the
// conversation that is about to continue.
//
// Each listed handle's sidecar file is stat'd before being described as
// "readable" (review finding N9): an operator can wipe the toolresults/
// directory, or a volume can roll back, out from under a live session, and
// the index must not assert readability it has not checked. A handle whose
// file is gone is still listed (its metadata is real and its retained-bytes
// accounting still holds it), just annotated as such.
func (s *Session) retainedResultsIndexPart() *message.Text {
	s.mu.Lock()
	var nums []int64
	for h := range s.toolResults {
		if n, ok := parseToolResultHandle(h); ok {
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		s.mu.Unlock()
		return nil
	}
	sortInt64s(nums)

	older := 0
	if len(nums) > retainedResultsIndexMaxHandles {
		older = len(nums) - retainedResultsIndexMaxHandles
		nums = nums[older:]
	}
	type row struct {
		meta   toolResultMeta
		handle string
	}
	rows := make([]row, 0, len(nums))
	for _, n := range nums {
		h := toolResultHandlePrefix + strconv.FormatInt(n, 10)
		rows = append(rows, row{meta: s.toolResults[h], handle: h})
	}
	s.mu.Unlock()

	var b strings.Builder
	b.WriteString("[retained tool results (this index is machine-generated, not part of the summary above):\n")
	for _, r := range rows {
		m := r.meta
		status := "still readable via read_tool_result"
		if _, err := os.Stat(s.toolResultPath(r.handle)); err != nil {
			status = "sidecar file missing, no longer readable"
		}
		fmt.Fprintf(&b, "  %s tool=%s bytes=%d lines=%d head=%q (%s)\n", m.Handle, m.Tool, m.Bytes, m.Lines, m.Head, status)
	}
	if older > 0 {
		fmt.Fprintf(&b, "  ...and %d older retained result(s) (not listed; still on disk if their sidecar files survive, addressable by handle number if known)\n", older)
	}
	b.WriteString("]")
	return &message.Text{Text: b.String()}
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
// openFileForToolResult is os.Open, indirected only so a test can force a
// non-missing-file error (permission denied, a transient I/O error) and
// confirm openRetainedToolResult does not misreport it as a permanent
// "gone" condition. Never reassigned outside a test.
var openFileForToolResult = os.Open

func (s *Session) openRetainedToolResult(handle string) (*os.File, error) {
	f, err := openFileForToolResult(s.toolResultPath(handle))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errToolResultFileMissing
	}
	if err != nil {
		// Anything OTHER than a genuine not-exist — permission denied, a
		// transient I/O error, EMFILE descriptor exhaustion — is NOT the
		// same as the file being gone, and must not get the same terminal
		// "no longer on disk ... it cannot be read back" wording: that
		// steers the model away from retrying a read that would succeed
		// once the transient condition clears. Return the raw error; the
		// caller's generic "cannot read handle %q" fallback (runReadToolResult)
		// already handles it without carrying an absolute filesystem path.
		return nil, err
	}
	return f, nil
}
