// Command gen writes protocol/openapi.json and protocol/protocol.ts from the
// route table of internal/server and the Go types that it names.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", ".", "directory that receives openapi.json and protocol.ts")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	doc, ts, err := render()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "openapi.json"), doc, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "protocol.ts"), ts, 0o644)
}
