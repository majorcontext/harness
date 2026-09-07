package server

import "net/http"

type tipJSON struct {
	Seq int64 `json:"seq"`
}

// handleEventTip serves GET /event/tip: the box-global journal tip, so a
// consumer can ask "have I seen everything" without opening the stream and
// replaying it.
func (s *Server) handleEventTip(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, tipJSON{Seq: s.currentSeq()})
}
