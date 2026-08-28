package server

import (
	"errors"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/plugin"
)

// defaultJournalLimit/maxJournalLimit bound GET /session/{id}/journal's
// `limit` query parameter: unset or non-positive falls back to
// defaultJournalLimit, and anything larger is clamped to maxJournalLimit —
// this is a debugging endpoint, not a hot path, but an unbounded page size
// would still let one request read an entire multi-thousand-record log in
// one response.
const (
	defaultJournalLimit = 200
	maxJournalLimit     = 1000
)

// JournalResponse is GET /session/{id}/journal's body: one page of the
// session's durable log (engine.JournalRecord), oldest first. NextCursor is
// the Seq to pass as the next request's `from` to continue paging; HasMore
// is true only when this page was actually truncated by `limit` (mirrors
// the SSE stream's own `from`-cursor convention, server/sse.go's
// parseFrom/handleEvent — a Seq is stable across reloads since the log is
// append-only, so it doubles as a pagination cursor here too).
type JournalResponse struct {
	SessionID  string                 `json:"session_id"`
	Records    []engine.JournalRecord `json:"records"`
	NextCursor int                    `json:"next_cursor,omitempty"`
	HasMore    bool                   `json:"has_more,omitempty"`
}

// handleJournal implements GET /session/{id}/journal: the session's own
// durable engine log (engine.LoadJournal), reshaped and sanitized, oldest
// first — read-only, paginated via `from`/`limit` query parameters
// (mirroring the SSE stream's `from` cursor convention). This is the
// endpoint that restart-recovery debugging for PR #145 and PR #147 kept
// needing pod-exec into a box to answer by hand
// — "was a task-notification checkout/commit/requeue ever recorded for this
// child" or "did a recovery marker fire on this turn" — now answerable over
// the wire.
//
// id must be a session this process actually knows about (resident or
// disk-loadable via s.lookup, exactly like handleGet) — an unknown id is a
// plain 404, never a distinct "session unknown vs. journal empty" signal,
// matching every other per-{id} route's not-found shape in this file. Once
// the session is confirmed to exist, a session log that has not been
// persisted YET (Session.Persist's own "nothing touches disk until the
// first message append" rule, or the Spawn race window s.lookup's own doc
// comment describes) answers 200 with an empty page — never a 404 — since
// the session genuinely exists, it simply has no durable records yet.
func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	// s.lookupSession's own cold-load path (LoadSession: full ReadFile + scanLog +
	// message replay + orphan repair) is discarded here — engine.LoadJournal
	// below re-reads and re-parses the SAME file from scratch, and every
	// subsequent page of a long log re-reads the whole file again
	// (paginateJournal slices an already-fully-loaded, already-projected
	// []JournalRecord). This is a debug endpoint, not a hot path, so the
	// redundant I/O is an accepted tradeoff rather than something worth a
	// cache or a leaner existence check.
	if _, ok := s.lookupSession(id); !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}

	records, err := engine.LoadJournal(s.opts.SessionDir, id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeJSON(w, http.StatusOK, JournalResponse{SessionID: id, Records: []engine.JournalRecord{}})
			return
		}
		// Sanitized, matching engine/journal.go's own package-level rule
		// ("every field that can carry a raw provider/tool error string is
		// sanitized ... before it ever leaves this package") — this error
		// originates OUTSIDE that package (a corrupt-log JSON decode error,
		// or an OS-level ReadFile failure) but the same boundary applies:
		// nothing written by this handler skips SanitizeSessionError.
		writeErr(w, http.StatusInternalServerError, plugin.SanitizeSessionError(err.Error()))
		return
	}

	page, nextCursor, hasMore := paginateJournal(records, parseJournalFrom(r), parseJournalLimit(r))
	writeJSON(w, http.StatusOK, JournalResponse{
		SessionID:  id,
		Records:    page,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	})
}

// parseJournalFrom resolves the `from` query parameter (an exclusive lower
// bound on Seq): absent or unparseable defaults to 0 (from the start),
// mirroring parseFrom's identical leniency for the SSE stream's own `from`
// (server/sse.go) rather than rejecting a malformed value with a 400.
func parseJournalFrom(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("from"))
	if n < 0 {
		return 0
	}
	return n
}

// parseJournalLimit resolves the `limit` query parameter, defaulting and
// clamping into [1, maxJournalLimit] -- see those constants' own doc
// comment.
func parseJournalLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return defaultJournalLimit
	}
	if n > maxJournalLimit {
		return maxJournalLimit
	}
	return n
}

// paginateJournal returns the slice of records with Seq > from, capped at
// limit entries: page (never nil, so the wire shape is always `[]`, never
// `null`, matching writeJSON's other list responses), nextCursor (the last
// returned record's Seq, or from unchanged if the page is empty), and
// hasMore (true only when capping actually truncated the result — a page
// that already reaches the end of the log reports false, even though a
// caller could harmlessly re-request with nextCursor and get an empty page
// back).
func paginateJournal(records []engine.JournalRecord, from, limit int) (page []engine.JournalRecord, nextCursor int, hasMore bool) {
	page = []engine.JournalRecord{}
	nextCursor = from
	for _, rec := range records {
		if rec.Seq <= from {
			continue
		}
		if len(page) == limit {
			hasMore = true
			break
		}
		page = append(page, rec)
	}
	if len(page) > 0 {
		nextCursor = page[len(page)-1].Seq
	}
	return page, nextCursor, hasMore
}
