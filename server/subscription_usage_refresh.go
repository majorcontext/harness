package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// subscriptionUsageRefreshResponseJSON is the POST
// /session/{id}/subscription-usage/refresh response shape. SubscriptionUsage
// is always the session's current cached snapshot (sessionJSON's own field
// of the same wire name) whether or not this call reached a provider:
// Supported distinguishes a genuine live read (true) from a session whose
// current provider has no on-demand read (false, provider.
// ErrSubscriptionUsageRefreshUnsupported) — a documented outcome, not a
// failure, so a caller can fall back to its ordinary bootstrap re-read
// without rendering an error.
type subscriptionUsageRefreshResponseJSON struct {
	SubscriptionUsage *message.SubscriptionUsage `json:"subscription_usage"`
	Supported         bool                       `json:"supported"`
}

// handleSubscriptionUsageRefresh answers an on-demand subscription-usage
// read for a session's current provider (see engine.Session.
// RefreshSubscriptionUsage and provider.SubscriptionUsageRefresher).
// Mirrors handleSetThinking/handleSetServiceTier's session resolution: a
// managed child comes straight from SessionManager's own resident node, a
// root loads and caches a cold one with the same eviction handling.
func (s *Server) handleSubscriptionUsageRefresh(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}

	var sess *engine.Session
	if child, ok := s.sessMgr.Session(id); ok && child.TaskParentID() != "" {
		sess = child
	} else {
		s.mu.Lock()
		st := s.sessions[id]
		s.mu.Unlock()
		if st == nil {
			loaded, err := s.opts.LoadSession(id)
			if err != nil {
				writeErr(w, http.StatusNotFound, "no such session")
				return
			}
			s.mu.Lock()
			var evicted []*engine.Session
			if ex := s.sessions[id]; ex != nil {
				st = ex
			} else {
				st = &sessionState{sess: loaded, lastUsed: time.Now()}
				s.sessions[id] = st
				evicted = s.evictResidentLocked()
			}
			s.mu.Unlock()
			releaseEvicted(evicted)
		}
		sess = st.sess
	}

	usage, err := sess.RefreshSubscriptionUsage(r.Context())
	if err != nil {
		if errors.Is(err, provider.ErrSubscriptionUsageRefreshUnsupported) {
			writeJSON(w, http.StatusOK, subscriptionUsageRefreshResponseJSON{
				SubscriptionUsage: sess.SubscriptionUsage(),
				Supported:         false,
			})
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, subscriptionUsageRefreshResponseJSON{
		SubscriptionUsage: usage,
		Supported:         true,
	})
}
