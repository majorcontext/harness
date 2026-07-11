//go:build !unix

package server

import (
	"context"

	"github.com/coder/websocket"
)

// ptySupported is false on every non-unix GOOS (windows, plan9, js/wasm,
// wasip1): there is no unix PTY there. handleTerm checks this BEFORE ever
// calling websocket.Accept, so a request to /term on these platforms gets a
// plain 501 — no upgrade, no shell, nothing to tear down. This mirrors
// engine/bash_other.go's configureProcessGroup stub: it exists purely so
// this package keeps cross-compiling for every GOOS per AGENTS.md, not just
// the big two.
const ptySupported = false

// runTerminal is unreachable in production on a !unix GOOS (ptySupported
// gates the only call site in handleTerm), but must still exist and
// compile so the package builds. Its body is defensive, not load-bearing.
func runTerminal(_ context.Context, conn *websocket.Conn, _ string) {
	conn.Close(websocket.StatusInternalError, "interactive terminals are unix-only")
}
