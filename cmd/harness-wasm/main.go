//go:build js

package main

import "syscall/js"

func main() {
	js.Global().Set("harnessConnect", js.FuncOf(harnessConnect))
	select {}
}
