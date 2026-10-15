// Command embed-server is a reference server for a page that runs the harness
// WebAssembly module with auth "injected". It serves the module and the page,
// holds the ChatGPT login of each user, and adds the credential to the model
// requests of the page with a gatekeeper relay. See README.md.
package main

import (
	"flag"
	"log"
	"mime"
	"net"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	wasm := flag.String("wasm", "harness.wasm", "path of the harness WebAssembly module")
	wasmExec := flag.String("wasm-exec", "wasm_exec.js", "path of wasm_exec.js of the Go version that built the module")
	embed := flag.String("embed", "../../sdk/embed/src", "directory of the sdk/embed sources")
	upstream := flag.String("upstream", "https://chatgpt.com/backend-api/codex", "Codex backend")
	secure := flag.String("secure", "auto", "Secure on the session cookie: true, false, or auto (true unless -addr is on a loopback address)")
	tokenFile := flag.String("token-file", "", "file that keeps the ChatGPT logins across restarts, mode 0600, in plain text: for development only")
	flag.Parse()

	_ = mime.AddExtensionType(".mjs", "text/javascript")
	s, err := newServer(config{Upstream: *upstream, WasmPath: *wasm, WasmExecPath: *wasmExec, EmbedDir: *embed, SecureCookie: secureCookie(*secure, *addr), TokenFile: *tokenFile, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on http://%s/", *addr)
	log.Fatal((&http.Server{Addr: *addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}

// secureCookie decides the Secure flag of the session cookie. A cookie that
// carries it never travels over plain http, so auto turns it on for anything
// that is not a loopback listener.
func secureCookie(flagValue, addr string) bool {
	switch flagValue {
	case "true":
		return true
	case "false":
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}
