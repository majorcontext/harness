package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// subscriber is one connected SSE client.
type subscriber struct {
	ch      chan Event
	session string // "" = all sessions
}

const defaultHeartbeat = 30 * time.Second

// handleEvent serves the instance-wide SSE stream: it replays durable journal
// records with seq greater than the requested cursor (in order), then streams
// live events. Registration and the replay snapshot happen under one lock, so
// no record can slip through the gap between them — replay covers (from, max]
// and every later event (seq > max) is delivered live.
func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	from := parseFrom(r)
	filter := r.URL.Query().Get("session")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := &subscriber{ch: make(chan Event, 256), session: filter}
	s.mu.Lock()
	var replay []Event
	for _, ev := range s.journal {
		if ev.Seq > from && (filter == "" || ev.SessionID == filter) {
			replay = append(replay, ev)
		}
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
	}()

	interval := s.opts.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeat
	}
	s.stream(r.Context(), w, flusher, sub, replay, interval)
}

// parseFrom resolves the replay cursor. The `from` query parameter wins; the
// standard Last-Event-ID reconnection header is the fallback when `from` is
// absent.
func parseFrom(r *http.Request) int64 {
	raw := r.URL.Query().Get("from")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	n, _ := strconv.ParseInt(raw, 10, 64)
	return n
}

// stream writes the replay batch, then multiplexes live events and periodic
// heartbeat comments until the client disconnects. The heartbeat interval is
// injectable so timer behavior is testable without wall-clock waits.
func (s *Server) stream(ctx context.Context, w io.Writer, flusher http.Flusher, sub *subscriber, replay []Event, interval time.Duration) {
	for _, ev := range replay {
		writeSSE(w, ev)
		flusher.Flush()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sub.ch:
			writeSSE(w, ev)
			flusher.Flush()
		case <-ticker.C:
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// writeSSE encodes one event as an SSE frame. Durable records set the id:
// field to their seq so Last-Event-ID reconnection works with standard clients.
func writeSSE(w io.Writer, ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if ev.Seq != 0 {
		fmt.Fprintf(w, "id: %d\n", ev.Seq)
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}
