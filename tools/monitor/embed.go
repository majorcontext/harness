// Package monitor embeds the monitor's single committed file
// (tools/monitor/index.html) so a `harness serve` box can serve its own
// copy at GET /monitor (see server.Options' MonitorPage field and
// cmd/harness's serveCmd wiring). The page itself has no Go-side logic of
// its own — same "no build step, no external resources" design as
// tools/hub (see tools/hub/hub.go's own //go:embed) — this package exists
// solely to make the committed file embeddable without server importing
// tools/* (server stays a pure HTTP+SSE layer over the engine; cmd/harness
// is the one place that wires a tools/* page into it, exactly as it
// already does for `harness hub` — see main.go's import of tools/hub).
package monitor

import _ "embed"

// Page is the exact, committed tools/monitor/index.html, byte for byte —
// the SAME file that can also be opened directly via file:// or served
// from any static host (see the file's own header comment); embedding it
// here does not change or replace that path, it adds a third way to serve
// it. cmd/harness's serveCmd hands this to server.Options.MonitorPage,
// which — when non-nil — serves it unauthenticated at GET /monitor,
// same-origin with the box's own API (see that field's doc comment for
// why unauthenticated is correct here).
//
//go:embed index.html
var Page []byte
