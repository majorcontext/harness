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
	if limit <= 0 {
		limit = DefaultMessagePageLimit
	}
	if limit > MaxMessagePageLimit {
		limit = MaxMessagePageLimit
	}
	page := MessagePage{Total: ix.DurableMessages}
	hi := ix.DurableMessages
	if beforeSeq > 0 && beforeSeq-1 < hi {
		hi = beforeSeq - 1
	}
	if hi < 1 {
		return page, nil
	}
	lo := hi - limit + 1
	if lo < 1 {
		lo = 1
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
func tailPage(f *os.File, logSize, size int64, total, lo, hi int) ([]message.Message, bool, error) {
	cur := total
	var out []message.Message
	compacted := false

	err := scanLogBackward(f, logSize, size, func(line []byte, isTail bool) (bool, error) {
		var rec struct {
			Type    string           `json:"type"`
			Message *message.Message `json:"message,omitempty"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			if isTail {
				// A torn final line is a crash mid-write. scanLog's forward
				// discipline ignores it; so does this one, and the index
				// this page is numbered against ignored it too.
				return true, nil
			}
			return false, errors.New("corrupt record")
		}
		switch rec.Type {
		case recCompact:
			compacted = true
			return false, nil
		case recMessage:
			if rec.Message == nil {
				return true, nil
			}
			if cur >= lo && cur <= hi {
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
// searches backwards for a newline; the line it delimits is then read once,
// by offset, at its exact length. An earlier version instead prepended each
// block to a carry buffer, which copies the whole partial line per block: a
// single 20 MB record spans hundreds of blocks and copied gigabytes before
// decoding one message. Journals carry records that large — a tool result
// or an image Blob — so the cost was reachable from an ordinary page
// request.
func scanLogBackward(f *os.File, end, size int64, fn func(line []byte, isTail bool) (bool, error)) error {
	if end > size {
		end = size
	}
	if end <= 0 {
		return nil
	}
	// lineEnd is the offset just past the line under consideration. Every
	// read below stays inside [0, end), so a journal that grew after the
	// caller chose end is never read past it.
	lineEnd := end
	block := make([]byte, revChunkBytes)
	first := true
	for lineEnd > 0 {
		// Find the newline that starts this line, walking backwards a block
		// at a time.
		lineStart := int64(0)
		searchEnd := lineEnd
		for searchEnd > 0 {
			n := int64(revChunkBytes)
			if n > searchEnd {
				n = searchEnd
			}
			pos := searchEnd - n
			buf := block[:n]
			if _, err := f.ReadAt(buf, pos); err != nil {
				return err
			}
			if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
				lineStart = pos + int64(i) + 1
				break
			}
			searchEnd = pos
		}
		if lineStart >= lineEnd {
			// An empty line: the terminator of the line before it. Step
			// over it without consuming the one torn-line allowance.
			lineEnd = lineStart - 1
			continue
		}
		line := make([]byte, lineEnd-lineStart)
		if _, err := f.ReadAt(line, lineStart); err != nil {
			return err
		}
		lineEnd = lineStart - 1 // step over the newline itself
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			cont, err := fn(trimmed, first)
			first = false
			if err != nil || !cont {
				return err
			}
		}
	}
	return nil
}
