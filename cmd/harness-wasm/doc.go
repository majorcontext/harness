//go:build js

// Command harness-wasm is the entry of the WebAssembly module. It builds with
// GOOS=js GOARCH=wasm and runs inside a JavaScript host, which calls
// globalThis.harnessConnect to open one ACP connection.
package main
