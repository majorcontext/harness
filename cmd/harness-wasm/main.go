//go:build js

// Command harness-wasm is the entry of the WebAssembly module. It builds with
// GOOS=js GOARCH=wasm and runs inside a JavaScript host.
package main

import (
	"log"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/store"
)

func main() {
	if _, err := harness.New(harness.Options{Store: store.NewMemStore()}); err != nil {
		log.Fatal(err)
	}
	select {}
}
