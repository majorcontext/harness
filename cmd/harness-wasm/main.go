//go:build js

package main

import (
	"os"
	"syscall/js"
)

func main() {
	js.Global().Set("harnessConnect", js.FuncOf(harnessConnect))
	js.Global().Set("harnessExit", js.FuncOf(func(js.Value, []js.Value) any {
		os.Exit(0)
		return nil
	}))
	select {}
}
