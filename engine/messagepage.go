// Paginated message reads: the newest K messages of a session, or the K
// before a given sequence number, read from the journal's tail instead of
// its whole length.
//
// The problem. GET /session/{id}/message answered with the ENTIRE message
// history, always. On the fleet's longest production session that is 1.4 MB
// and 5.8 s per load, every time a console opens (meetneptune/boxes
// docs/design/console-read-path.md, workstream 2). A console shows the tail
// first and pages older messages in on scroll, so it needs a bounded read.
//
// The sequence number. A message's seq is its 1-based ordinal in the
// session's DURABLE message sequence: message records in log order, with
// each compact record's fold applied (the folded range replaced by that
// record's summary). SessionIndex.DurableMessages (index.go) is the count
// of that same sequence, so the newest message's seq equals it. Note that
// SessionIndex.Messages can be LARGER: it also counts the synthetic tool
// results message.ResolveOrphanToolCalls derives for a tool call whose
// result never reached the log. Those messages have no record, so they have
// no seq and no page carries them. This definition is
// what makes a bounded read possible at all: an ordinal over the durable
// records can be counted backwards from the end, while an ordinal over a
// materialized history cannot be known without materializing it.
//
// Two consequences follow, and both are deliberate:
//
//   - A page carries the durable messages VERBATIM. It never runs
//     message.ResolveOrphanToolCalls, the load-time repair that synthesizes
//     an is_error tool result for a tool call whose result is absent. That
//     repair exists so a REQUEST is wire-valid; this endpoint builds no
//     request. Running it here would also fabricate a failure at every page
//     boundary that splits an assistant message from its results, and a
//     fabricated tool failure in a read view is a defect with production
//     history (see Server.lookup's doc comment, server/handlers.go: a
//     healthy child's in-flight tool call rendered as failed in the console
//     for as long as it kept running).
//   - Compaction renumbers. A fold replaces N messages with one summary, so
//     every seq after it shifts down by N-1. A client paging across a
//     compaction can see one page overlap another; message ids, which are
//     stable, are the way to de-duplicate.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/majorcontext/harness/message"
)

// DefaultMessagePageLimit is the page size a caller gets when it asks for a
// page without naming one. MaxMessagePageLimit caps what it may ask for:
// the point of this API is a bounded read, so an unbounded limit is a
// contradiction, and a caller that truly wants everything still has the
// unparameterized call.
const (
	DefaultMessagePageLimit = 100
	MaxMessagePageLimit     = 1000
)

// ErrStaleMessagePage reports that the journal changed under a page read in
// a way that invalidates the sequence numbers it was about to serve: it is
// now shorter than the index that numbered it. A caller retries with a
// fresh index; ReadMessagePage does that itself once (see readMessagePage).
var ErrStaleMessagePage = errors.New("engine: session journal changed under a message page read")

// MessagePage is one page of a session's durable message sequence, oldest
// first — the same order the unparameterized read returns.
type MessagePage struct {
	// Messages holds the page, ascending by seq. Empty when the session has
	// no messages at or below the requested point.
	Messages []message.Message
	// FirstSeq and LastSeq are the seqs of the first and last entries of
	// Messages, and 0 for an empty page. A client pages further back by
	// asking again with BeforeSeq = FirstSeq.
	FirstSeq int
	LastSeq  int
	// Total is the session's whole durable message count — the seq of its
	// newest message — so a client knows where the page sits without a
	// second call. It can be lower than the `messages` field of GET
	// /session, which also counts derived repair messages that have no
	// record and so no seq.
	Total int
	// HasMore reports whether at least one message older than FirstSeq
	// exists. It is false for a page that starts at seq 1.
	HasMore bool
}

// revChunkBytes is the backward scan's read granularity. It is comfortably
// larger than one record of ordinary size, so a page of a few dozen
// messages usually costs one or two reads, and it bounds how much of a
// journal a page read touches at all.
const revChunkBytes = 64 << 10

// MessagePageWindow resolves a page request against a total, returning the
// inclusive sequence range [lo, hi] the page covers and the limit actually
// applied. hi < lo means an empty page: nothing sits at or below the
// requested point.
//
// beforeSeq <= 0 means "the newest page". limit <= 0 means
// DefaultMessagePageLimit, and a limit above MaxMessagePageLimit is capped
// to it — an engine caller gets a bounded answer rather than an error. The
// HTTP boundary is stricter: it rejects an oversized limit, because its
// published schema names a maximum and a client generator enforces it.
//
// It is exported because the server computes the same window when it pages
// a resident history for a session with no journal. Two copies of this
// arithmetic drifting apart would give one session two different
// paginations depending on which path answered.
func MessagePageWindow(total, beforeSeq, limit int) (lo, hi, appliedLimit int) {
	if limit <= 0 {
		limit = DefaultMessagePageLimit
	}
	if limit > MaxMessagePageLimit {
		limit = MaxMessagePageLimit
	}
	hi = total
	if beforeSeq > 0 && beforeSeq-1 < hi {
		hi = beforeSeq - 1
	}
	if hi < 1 {
		return 1, 0, limit // empty page
	}
	lo = hi - limit + 1
	if lo < 1 {
		lo = 1
	}
	return lo, hi, limit
}

// ReadMessagePage returns the durable messages immediately BEFORE
// beforeSeq, at most limit of them, reading only the journal bytes it needs.
//
// beforeSeq <= 0 means "the newest page". limit <= 0 means
// DefaultMessagePageLimit, and a limit above MaxMessagePageLimit is capped
// to it rather than rejected — a caller asking for too much gets a bounded
// answer, not an error it has to handle.
//
// The scan is bounded by SessionIndex.LogSize, not by the file's current
// size, which is what keeps a page consistent with the Total it reports: a
// turn appending records while this runs cannot renumber the page under it,
// because those bytes are past the end this read agreed to look at.
func ReadMessagePage(dir, id string, beforeSeq, limit int) (MessagePage, error) {
	page, err := readMessagePage(dir, id, beforeSeq, limit)
	if errors.Is(err, ErrStaleMessagePage) {
		// One retry, with a freshly folded index. A journal shrinks only
		// when a torn tail is repaired, which happens once per crash, so a
		// second stale answer is a real fault rather than a race.
		page, err = readMessagePage(dir, id, beforeSeq, limit)
	}
	return page, err
}

// readMessagePage takes the index through ReadSessionIndex, which memoizes
// a refold. That write is deliberate here and not the anti-pattern a
// LISTING has: this is one session, and without it every page request for
// a session whose sidecar is stale refolds the whole journal again.
//
// It can overlap the session's own writer. The overlap is bounded and
// benign: each writer writes a COMPLETE index of the prefix it folded,
// carrying that prefix's own staleness key, and the checksum covers the
// bytes (see sessionIndexFile), so a reader sees a file that is current or
// visibly stale, never a blend. The window is also small — Session.
// writeRecord's append and its flush are two steps under one lock — so a
// page read only refolds when its stat and its sidecar read straddle that
// gap.
func readMessagePage(dir, id string, beforeSeq, limit int) (MessagePage, error) {
	ix, err := ReadSessionIndex(dir, id)
	if err != nil {
		return MessagePage{}, err
	}
	return readMessagePageWithIndex(dir, id, ix, beforeSeq, limit)
}

// readMessagePageWithIndex is readMessagePage with the index already in
// hand. The split is a test seam: the window this function's own stale
// checks exist for opens between taking an index and reading the journal,
// which nothing outside can drive through the public call.
func readMessagePageWithIndex(dir, id string, ix SessionIndex, beforeSeq, limit int) (MessagePage, error) {
	page := MessagePage{Total: ix.DurableMessages}
	lo, hi, _ := MessagePageWindow(ix.DurableMessages, beforeSeq, limit)
	if hi < lo {
		return page, nil
	}

	f, err := os.Open(sessionPath(dir, id))
	if err != nil {
		return MessagePage{}, err
	}
	defer f.Close()

	// The index named a journal length; the file has to still be at least
	// that long. It can be shorter: another process's ensureLog repairs a
	// torn tail by truncating it. A read that started BEFORE such a repair
	// would number its page against records that no longer exist, so this
	// reports staleness and the caller takes a fresh index.
	//
	// This check and pageError below overlap on purpose. This one answers
	// the cheap, common case before any scan runs. pageError answers the
	// narrower window the check cannot cover: a repair that lands after it
	// and before the scan reads. Removing either leaves the other reporting
	// the same classification, one attempt later.
	fi, err := f.Stat()
	if err != nil {
		return MessagePage{}, err
	}
	if fi.Size() < ix.LogSize {
		return MessagePage{}, ErrStaleMessagePage
	}

	msgs, ok, err := tailPage(f, ix.LogSize, fi.Size(), ix.DurableMessages, lo, hi)
	if err != nil {
		return MessagePage{}, pageError(f, id, ix, err)
	}
	if !ok {
		// The page reaches into compacted history. Fall back to the forward
		// fold, over exactly the bytes the index summarized, so the page
		// and its seqs describe one instant of the journal.
		//
		// The read is bounded by LogSize, not by the file's current size: a
		// turn appending a large record while this runs must not enlarge
		// the buffer, and bytes past LogSize are not part of the sequence
		// being numbered. It still holds the journal prefix in memory —
		// one slim pass, the same shape LoadSession and every refold
		// already use — which is why the tail walk above exists for pages
		// that do not need it.
		data := make([]byte, ix.LogSize)
		if _, err := io.ReadFull(f, data); err != nil {
			return MessagePage{}, pageError(f, id, ix, err)
		}
		if msgs, err = foldedPage(data, lo, hi); err != nil {
			return MessagePage{}, pageError(f, id, ix, err)
		}
	}
	page.Messages = msgs
	if n := len(msgs); n > 0 {
		page.FirstSeq = hi - n + 1
		page.LastSeq = hi
		page.HasMore = page.FirstSeq > 1
	}
	return page, nil
}

// pageError classifies a failure from a page scan. The check before the
// scan cannot close the whole window: another process's ensureLog can
// truncate a torn tail AFTER that check and BEFORE the scan reads. The scan
// then fails on bytes that no longer exist, and reports a numbering
// mismatch or a short read rather than the truth, which is that the index
// is stale. Re-stat and say so, and ReadMessagePage's one retry answers
// from a fresh index.
func pageError(f *os.File, id string, ix SessionIndex, cause error) error {
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() < ix.LogSize {
		return ErrStaleMessagePage
	}
	return fmt.Errorf("engine: session %s: %w", id, cause)
}

// tailPage is the fast path: a page whose whole range lies in the journal's
// UNCOMPACTED tail. Every message record there is one durable message, in
// order, so the walk numbers them down from total and stops as soon as the
// page is complete — touching only the tail of the file however long the
// journal is.
//
// It gives up (ok false) the moment it meets a compact record, because a
// compact record means the records older than it are not a plain sequence:
// the fold replaced a range of them with one summary, and the messages the
// fold KEPT sit in the log between that range and the record itself. Undoing
// that in reverse is exactly the kind of second, subtly different
// implementation of a fold this repository forbids, so the general path
// below reuses the forward fold instead.
func tailPage(src io.ReaderAt, logSize, size int64, total, lo, hi int) ([]message.Message, bool, error) {
	cur := total
	var out []message.Message
	compacted := false

	err := scanLogBackward(src, logSize, size, func(line logLine, isTail bool) (bool, error) {
		head, ok, err := classifyRecord(line, isTail)
		if err != nil {
			return false, err
		}
		if !ok {
			// A torn final record, which the fold that numbered this page
			// dropped too. Anything else would have failed that fold, so
			// the index this page reads would not exist.
			return true, nil
		}
		switch head.Type {
		case recCompact:
			compacted = true
			return false, nil
		case recMessage:
			if !head.hasMessage {
				// A message record with no body. No fold counts one: the
				// full fold tolerates the shape on a final line and skips
				// it, and the incremental write-path fold marks itself
				// broken and skips it too — and because that fold never
				// revisits an earlier record, a journal CAN carry a
				// bodyless record that is no longer final and still have a
				// usable index. Counting it here would shift every seq in
				// the page by one.
				return true, nil
			}
			if cur >= lo && cur <= hi {
				whole, err := line.All()
				if err != nil {
					return false, err
				}
				var rec struct {
					Message *message.Message `json:"message"`
				}
				if err := json.Unmarshal(bytes.TrimSpace(whole), &rec); err != nil || rec.Message == nil {
					return false, fmt.Errorf("message record at offset %d: %v", line.start, err)
				}
				msg := *rec.Message
				// The same ingest-time repair LoadSession applies to every
				// message it replays (message.Message.Normalize's doc
				// comment): an empty ToolResult persisted by an older
				// binary must not reach a reader unrepaired.
				msg.Normalize()
				out = append(out, msg)
			}
			cur--
		}
		return cur >= lo, nil
	})
	if err != nil {
		return nil, false, err
	}
	if compacted {
		return nil, false, nil
	}
	if len(out) != hi-lo+1 {
		// The walk ran out of journal before it produced the page it was
		// numbered for: the index and the records disagree. Report it
		// rather than serve messages under seqs that do not describe them.
		return nil, false, fmt.Errorf("message page [%d,%d]: journal holds %d of those messages", lo, hi, len(out))
	}
	reverseMessages(out)
	return out, true, nil
}

// recordHead is what a page walk needs to know about a record it is not
// going to carry: its type, and whether a message record has a body.
type recordHead struct {
	Type       string
	hasMessage bool
}

// classifyRecord decides what a line is, reading as little of it as it can
// WITHOUT guessing.
//
// It decodes into indexRecord — the type the fold decodes into — so the two
// agree by construction on every question: which records parse, which key
// wins when one is repeated, and whether a message record has a body. A
// classifier that answered any of those from a cheaper signal answered a
// different question, and a page numbered by one rule and walked by
// another serves real messages under wrong sequence numbers.
//
// The saving is that a record whose bytes already fit in the prefix — every
// ordinary record — is decoded from those bytes, with no second read. A
// record larger than the prefix window is read whole. It is still never
// MATERIALIZED: indexRecord carries indexMessage, which decodes a message's
// identity and skips its parts, so walking past a 20 MB image blob costs a
// scan of its bytes and no allocation of its content.
//
// An earlier revision classified from the prefix alone: the first key for
// the type, and a substring search for the message body. Both were
// unsound. A second top-level "type" key beyond the window resolves
// last-wins for the fold and first-wins for a prefix scan, and a nested
// "message" key made a bodyless record look body-bearing. Each produced a
// phantom message under a real sequence number. Reading a large record is
// the price of never doing that; the block window below is what keeps an
// ordinary journal cheap.
//
// ok is false only for a line that does not parse AND is the file's final
// line — a crash mid-write, which the forward scanner drops and this walk
// drops with it. A non-final line that does not parse is an error.
func classifyRecord(line logLine, isTail bool) (recordHead, bool, error) {
	raw := line.Prefix()
	if !line.Complete() {
		whole, err := line.All()
		if err != nil {
			return recordHead{}, false, err
		}
		raw = whole
	}
	head, parsed := decodeRecordHeadFull(raw)
	if parsed {
		return head, true, nil
	}
	if isTail {
		return recordHead{}, false, nil
	}
	return recordHead{}, false, errors.New("corrupt record")
}

// decodeRecordHeadFull decodes a whole record line into indexRecord — the
// SAME type the fold decodes into (see foldSessionJournal's scanLog call) —
// so parsed is false exactly where the fold's own decode fails, and
// hasMessage is exactly the fold's own test for a body.
//
// The type must stay indexRecord, not a slimmer shape that happens to carry
// the two fields this returns. The fold's tolerance is a property of EVERY
// field it type-checks: a record whose usage, goal, prompt, or compact
// payload has the wrong JSON shape fails that decode. A slimmer shape here
// ignores those fields, accepts the record, and counts a message the index
// never counted — a phantom that displaces a real message and shifts every
// seq in the page. Sharing the fold's type makes the two agree by
// construction rather than by a list of fields someone has to keep in step.
func decodeRecordHeadFull(raw []byte) (recordHead, bool) {
	var rec indexRecord
	if err := json.Unmarshal(bytes.TrimSpace(raw), &rec); err != nil {
		return recordHead{}, false
	}
	return recordHead{Type: rec.Type, hasMessage: rec.Message != nil}, true
}

// foldedPage is the general path, for a journal that carries at least one
// compact record. It folds the journal exactly as LoadSession does — the
// same indexFold, so the same applyCompactRecord — to learn WHICH message
// ids occupy seqs lo..hi, then decodes just those records.
//
// One pass, two decode depths. Every line is folded through indexRecord:
// ids, roles, timestamps, and tool-call ids, never a message body. The same
// pass keeps each record's raw line, keyed by the id that record
// contributes, as a subslice of data rather than a copy. Only the handful
// of lines a page actually carries is then decoded in full.
//
// The line map holds one entry per message record in the folded prefix, not
// one per message in the resulting sequence: a record a compaction folded
// away keeps its entry. That is deliberate. Each entry is an id string and
// a slice header beside a prefix this function already holds in memory in
// full, so pruning would trade a few percent of that for a pass per
// compaction.
//
// An id that appears on two records keeps the FIRST. Engine-minted ids are
// unique, and the one production source of a repeat is a provider-derived
// id hashed from the message's own text (message.ProviderCallID), where the
// two records carry the same content anyway. A journal that repeats an id
// with DIFFERENT content is damaged, and this renders the first of them
// rather than the last.
//
// An earlier revision ran a SECOND scanLog over the journal, decoding every
// line into a full record to find the wanted ones. That decoded every
// message body in the file, which is the cost this whole endpoint exists to
// avoid — a review caught it. The raw-line map is what removes it.
func foldedPage(data []byte, lo, hi int) ([]message.Message, error) {
	var fold indexFold
	// lineByID aliases data; it never copies a record.
	lineByID := make(map[string][]byte)
	err := scanLogRaw(data, func(line []byte, n int, isLast bool) error {
		var rec indexRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			if isLast {
				return errTruncatedFinalRecord
			}
			return fmt.Errorf("corrupt record at line %d: %v", n, err)
		}
		if err := fold.applyIndexRecord(rec, isLast); err != nil {
			return fmt.Errorf("%w at line %d", err, n)
		}
		var contributes string
		switch {
		case rec.Type == recMessage && rec.Message != nil:
			contributes = rec.Message.ID
		case rec.Type == recCompact && rec.Compact != nil:
			// A compact record contributes its summary to the sequence,
			// and the summary lives inside that record's own line.
			contributes = rec.Compact.Summary.ID
		}
		if contributes != "" {
			if _, seen := lineByID[contributes]; !seen {
				lineByID[contributes] = line
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if fold.broken {
		return nil, errors.New("message page: journal fold is not usable")
	}
	if hi > len(fold.messages) {
		return nil, fmt.Errorf("message page [%d,%d]: journal folds to %d messages", lo, hi, len(fold.messages))
	}
	out := make([]message.Message, hi-lo+1)
	for seq := lo; seq <= hi; seq++ {
		id := fold.messages[seq-1].ID
		line, ok := lineByID[id]
		if !ok {
			return nil, fmt.Errorf("message page [%d,%d]: no record for message %q at seq %d", lo, hi, id, seq)
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("message page [%d,%d]: message %q: %v", lo, hi, id, err)
		}
		var msg *message.Message
		switch {
		case rec.Type == recMessage:
			msg = rec.Message
		case rec.Type == recCompact && rec.Compact != nil:
			msg = &rec.Compact.Summary
		}
		if msg == nil {
			return nil, fmt.Errorf("message page [%d,%d]: record for message %q carries no message", lo, hi, id)
		}
		m := *msg
		// The same ingest-time repair LoadSession applies to every message
		// it replays (message.Message.Normalize's doc comment).
		m.Normalize()
		out[seq-lo] = m
	}
	return out, nil
}

// reverseMessages flips a newest-first slice into the oldest-first order
// every message response uses.
func reverseMessages(msgs []message.Message) {
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
}

// logLine is one line the backward scan found, handed to a callback as a
// PREFIX plus the means to read the rest.
//
// A page reader classifies most of the records it walks and keeps almost
// none of them: walking back to an older page passes every record after it.
// Reading each of those whole cost the bytes of a 20 MB image blob or tool
// result to learn a type string and drop it. The prefix answers that
// question, and All reads the body only for a record the page carries.
type logLine struct {
	src    io.ReaderAt
	start  int64
	length int64
	prefix []byte
}

// Prefix returns the line's first bytes — the whole line when it is short.
func (l logLine) Prefix() []byte { return l.prefix }

// Complete reports whether Prefix already holds the whole line.
func (l logLine) Complete() bool { return int64(len(l.prefix)) == l.length }

// All reads the whole line.
func (l logLine) All() ([]byte, error) {
	if l.Complete() {
		return l.prefix, nil
	}
	buf := make([]byte, l.length)
	if _, err := l.src.ReadAt(buf, l.start); err != nil {
		return nil, err
	}
	return buf, nil
}

// logLinePeekBytes bounds the prefix read. It is far larger than a record
// head — a type, and a message's identity fields — and far smaller than the
// large records this exists to avoid reading.
const logLinePeekBytes = 4 << 10

// scanLogBackward calls fn for each non-empty line in [0, end) of f, newest
// first, stopping when fn returns false or an error, or at byte 0. isTail is
// true only for the file's final line, which is the one a crash can leave
// torn — the same rule scanLog applies reading forward.
//
// size is the file's current length, which the caller has already stat'd;
// end is clamped to it, so a caller that names a bound from a stale index
// reads the file's real bytes rather than an EOF. A caller whose SEQUENCE
// NUMBERS depend on that bound must compare the two itself; ReadMessagePage
// does.
//
// It works in offsets, never in an accumulating buffer. Each block read
// searches backwards for a newline, and the line it delimits is then read
// once — bounded to logLinePeekBytes unless the callback asks for the rest.
// An earlier revision prepended each block to a carry buffer, which copies
// the whole partial line per block: one 20 MB record spans hundreds of
// blocks and copied gigabytes before decoding one message.
// src is an io.ReaderAt rather than an *os.File so a test can measure what
// a page read actually reads: the whole point of the prefix is that a deep
// page does not pull the bytes of the records it walks past, and only a
// counting reader can prove that.
func scanLogBackward(src io.ReaderAt, end, size int64, fn func(line logLine, isTail bool) (bool, error)) error {
	if end > size {
		end = size
	}
	if end <= 0 {
		return nil
	}
	// One block stays in memory, and the walk moves backwards through it.
	// Every line whose newline search and prefix fall inside it is served
	// without touching the file again. A block per LINE — which an earlier
	// revision did — reads a 64 KiB block to find a boundary 200 bytes
	// away, so a journal of small records was read several times over.
	win := newBackwardWindow(src)
	lineEnd := end
	first := true
	for lineEnd > 0 {
		lineStart, err := win.newlineBefore(lineEnd)
		if err != nil {
			return err
		}
		if lineStart >= lineEnd {
			// An empty line: the terminator of the line before it. Step
			// over it without consuming the one torn-line allowance.
			lineEnd = lineStart - 1
			continue
		}
		length := lineEnd - lineStart
		peek := length
		if peek > logLinePeekBytes {
			peek = logLinePeekBytes
		}
		prefix, err := win.at(lineStart, peek)
		if err != nil {
			return err
		}
		line := logLine{src: src, start: lineStart, length: length, prefix: prefix}
		lineEnd = lineStart - 1 // step over the newline itself
		blank := len(bytes.TrimSpace(prefix)) == 0
		if blank && !line.Complete() {
			// A prefix of whitespace does not make the LINE blank: a valid
			// record can begin with more leading space than the prefix
			// holds, and the forward scanner trims the whole line before
			// deciding. Read it before skipping it.
			whole, err := line.All()
			if err != nil {
				return err
			}
			blank = len(bytes.TrimSpace(whole)) == 0
		}
		if !blank {
			cont, err := fn(line, first)
			first = false
			if err != nil || !cont {
				return err
			}
		}
	}
	return nil
}

// backwardWindow holds one block of a file and serves reads that fall
// inside it, refilling backwards as a scan moves towards byte 0. It exists
// so a backward scan reads each byte of the span it walks about once.
type backwardWindow struct {
	src   io.ReaderAt
	buf   []byte
	start int64 // file offset of buf[0]
}

func newBackwardWindow(src io.ReaderAt) *backwardWindow {
	return &backwardWindow{src: src, buf: nil, start: -1}
}

// covers reports whether [off, off+n) lies inside the block in memory.
func (w *backwardWindow) covers(off, n int64) bool {
	return w.start >= 0 && off >= w.start && off+n <= w.start+int64(len(w.buf))
}

// fillEndingAt loads the block of up to revChunkBytes that ENDS at end.
func (w *backwardWindow) fillEndingAt(end int64) error {
	n := int64(revChunkBytes)
	if n > end {
		n = end
	}
	start := end - n
	buf := make([]byte, n)
	if _, err := w.src.ReadAt(buf, start); err != nil {
		return err
	}
	w.buf, w.start = buf, start
	return nil
}

// newlineBefore returns the offset just past the closest newline before
// end, or 0 when the span back to byte 0 holds none.
func (w *backwardWindow) newlineBefore(end int64) (int64, error) {
	searchEnd := end
	for searchEnd > 0 {
		if !w.covers(searchEnd-1, 1) {
			if err := w.fillEndingAt(searchEnd); err != nil {
				return 0, err
			}
		}
		// Search only the part of the block below searchEnd.
		hi := searchEnd - w.start
		if i := bytes.LastIndexByte(w.buf[:hi], '\n'); i >= 0 {
			return w.start + int64(i) + 1, nil
		}
		searchEnd = w.start
	}
	return 0, nil
}

// at returns n bytes at off, from the block when it covers them and from
// the file otherwise. The result is a copy: the block is refilled as the
// scan moves on.
func (w *backwardWindow) at(off, n int64) ([]byte, error) {
	out := make([]byte, n)
	if w.covers(off, n) {
		copy(out, w.buf[off-w.start:off-w.start+n])
		return out, nil
	}
	if _, err := w.src.ReadAt(out, off); err != nil {
		return nil, err
	}
	return out, nil
}
