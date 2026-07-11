// Package server: GET /term is an interactive terminal over WebSocket.
//
// SECURITY: this endpoint grants nothing the run token does not already
// grant — the bash tool (see engine/bash.go) already lets any authenticated
// caller execute arbitrary commands in the session's workdir. /term is the
// same trust boundary, just interactive (a live shell instead of one-shot
// command/response) and PTY-backed (so full-screen programs, job control,
// and line editing work). It is gated by the exact same run token as every
// other endpoint. See AGENTS.md's "Development hub" section for the
// operator-facing version of this note.
//
// WebSocket library choice: github.com/coder/websocket. It is a small,
// actively maintained, dependency-free RFC 6455 implementation with a
// context-based, io-idiomatic API (Read/Write take a context.Context,
// unlike gorilla/websocket's callback-free-but-context-free API) and native
// support for negotiating a specific subprotocol on Accept — exactly what
// the bearer-token-over-subprotocol auth trick below needs. We do not
// hand-roll the WS handshake/framing.
package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/coder/websocket"
)

// bearerSubprotocolPrefix is the Sec-WebSocket-Protocol prefix a browser
// client uses to carry the run token: browsers cannot set arbitrary headers
// (e.g. Authorization) on a WebSocket handshake request, but they can offer
// any subprotocol list they like, and the server can echo one back on
// accept. A non-browser client (e.g. a Go or curl-with-websocat test) may
// instead send a normal "Authorization: Bearer <token>" header — see
// authorized() in server.go, reused here unchanged.
//
// The token NEVER rides the URL/query string: unlike a header or
// subprotocol, a query string is what ends up in access logs, proxy logs,
// and browser history, so accepting it there would leak the run token to
// exactly the places auth secrets must never appear.
const bearerSubprotocolPrefix = "bearer."

// bearerSubprotocolToken extracts the token from a Sec-WebSocket-Protocol
// header value (a comma-separated list of subprotocols the client offered),
// returning the first one prefixed "bearer.". ok is false when no such
// subprotocol was offered at all (a non-browser client is expected to use
// the Authorization header instead — see handleTerm).
func bearerSubprotocolToken(header string) (token string, ok bool) {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(p)
		if t, found := strings.CutPrefix(p, bearerSubprotocolPrefix); found {
			return t, true
		}
	}
	return "", false
}

// handleTerm upgrades to a WebSocket and relays a PTY-backed shell:
// PTY output -> binary frames, client binary frames -> PTY input, client
// TEXT frames -> JSON control messages (currently just {"type":"resize",
// "cols":N,"rows":N}, applied to the PTY winsize). See the package doc
// comment above for the security framing and the WebSocket library choice.
//
// Auth and Origin are both checked, and the optional session's workdir is
// resolved, BEFORE the WebSocket is ever accepted — so a bad token or a
// mismatched Origin never gets so far as spawning a shell, let alone a PTY
// (see TestTermBadTokenRejectedBeforeSpawn / TestTermOriginMismatchRejected).
func (s *Server) handleTerm(w http.ResponseWriter, r *http.Request) {
	subToken, hasSub := bearerSubprotocolToken(r.Header.Get("Sec-WebSocket-Protocol"))
	authed := false
	echoProtocol := ""
	if hasSub && subtle.ConstantTimeCompare([]byte(subToken), []byte(s.opts.RunToken)) == 1 {
		authed = true
		echoProtocol = bearerSubprotocolPrefix + subToken
	} else if s.authorized(r) {
		authed = true
	}
	if !authed {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// The WebSocket handshake itself is not subject to the browser's CORS
	// preflight machinery (there is no preflight for WebSocket upgrades at
	// all), so a malicious page could otherwise open a cross-origin
	// WebSocket straight to /term and ride the operator's own browser as a
	// confused deputy. Enforce the SAME -cors-origin allowlist ServeHTTP
	// already applies to ordinary requests: an Origin header present and
	// not matching is rejected outright. A request with no Origin header at
	// all (any non-browser client) is unaffected — Origin is a browser-only
	// header.
	if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin) {
		writeErr(w, http.StatusForbidden, "origin not allowed")
		return
	}

	workDir, err := s.termWorkDir(r.URL.Query().Get("session"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	if !ptySupported {
		// See term_other.go: every non-unix GOOS. Respond plainly, without
		// ever calling websocket.Accept — no upgrade, no PTY, nothing to
		// tear down.
		writeErr(w, http.StatusNotImplemented, "interactive terminals are unix-only (GOOS="+runtime.GOOS+")")
		return
	}

	acceptOpts := &websocket.AcceptOptions{
		// Origin was already validated by hand above against -cors-origin
		// (which, unlike coder/websocket's OriginPatterns, also covers the
		// "no -cors-origin configured at all" case by rejecting every
		// browser Origin outright); skip the library's own origin check so
		// the two don't have to be kept in sync.
		InsecureSkipVerify: true,
	}
	if echoProtocol != "" {
		acceptOpts.Subprotocols = []string{echoProtocol}
	}
	conn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		// Accept already wrote its own error response.
		return
	}
	runTerminal(r.Context(), conn, workDir)
}

// originAllowed mirrors the allow-origin decision ServeHTTP's CORS layer
// makes for ordinary requests (see Options.CORSOrigin's doc comment):
// CORSOrigin == "*" allows everything, otherwise origin must match exactly.
// An empty CORSOrigin (CORS disabled entirely) allows nothing here either —
// there is no configured origin a browser page could ever legitimately be.
func (s *Server) originAllowed(origin string) bool {
	if s.opts.CORSOrigin == "*" {
		return true
	}
	return s.opts.CORSOrigin != "" && origin == s.opts.CORSOrigin
}

// termWorkDir resolves /term's optional session=<id> query param to a
// working directory: the named session's own WorkDir() (which, for a
// 'worktree'-isolation session, is its dedicated git worktree path — see
// worktree.go) when given, or the server process's own current working
// directory when the param is absent. It reuses lookup (see handlers.go),
// the same resident-or-load-from-disk resolution every other read endpoint
// uses, so a /term session=<id> works for a session this process loaded
// cold just as well as one it created.
func (s *Server) termWorkDir(sessionID string) (string, error) {
	if sessionID == "" {
		return os.Getwd()
	}
	sess, _, ok := s.lookup(sessionID)
	if !ok {
		return "", fmt.Errorf("no such session %q", sessionID)
	}
	return sess.WorkDir(), nil
}
