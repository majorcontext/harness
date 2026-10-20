package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
)

// wasmModule serves the module read at startup. The compressed bodies are
// made in the background (brotli at quality 11 takes about a minute for the
// full module); until one exists, the handler serves the next best body.
type wasmModule struct {
	raw      []byte
	hash     string
	gzip, br atomic.Pointer[[]byte]
	// done closes when both bodies exist.
	done chan struct{}
}

func newWasmModule(path string) (*wasmModule, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	m := &wasmModule{raw: raw, hash: hex.EncodeToString(sum[:8]), done: make(chan struct{})}
	go m.compress()
	return m, nil
}

func (m *wasmModule) compress() {
	defer close(m.done)
	var g bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&g, gzip.BestCompression)
	_, _ = zw.Write(m.raw)
	_ = zw.Close()
	gz := g.Bytes()
	m.gzip.Store(&gz)

	var b bytes.Buffer
	bw := brotli.NewWriterOptions(&b, brotli.WriterOptions{Quality: 11, LGWin: 24})
	_, _ = bw.Write(m.raw)
	_ = bw.Close()
	br := b.Bytes()
	m.br.Store(&br)
}

func accepts(r *http.Request, coding string) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if name == coding {
			q, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(params), "q="), 64)
			return params == "" || err != nil || q > 0
		}
	}
	return false
}

func (m *wasmModule) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, coding := m.raw, ""
	if b := m.br.Load(); b != nil && accepts(r, "br") {
		body, coding = *b, "br"
	} else if g := m.gzip.Load(); g != nil && accepts(r, "gzip") {
		body, coding = *g, "gzip"
	}
	h := w.Header()
	h.Set("Content-Type", "application/wasm")
	h.Set("Vary", "Accept-Encoding")
	h.Set("Cache-Control", "no-cache")
	etag := m.hash
	if coding != "" {
		h.Set("Content-Encoding", coding)
		etag += "-" + coding
	}
	h.Set("ETag", `"`+etag+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}
