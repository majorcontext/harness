// Command hubverify starts the REAL box + hub servers from tools/hub/e2e
// (see that package's doc comment) and prints their addresses/token as one
// line of JSON, then blocks forever. It exists for manual, by-hand
// verification of the hub page against a real backend — e.g. to run
// real_e2e.mjs directly, or to open a browser at the printed hub_base and
// click around by hand:
//
//	go run ./tools/hub/e2e/hubverify
//	# -> {"box_base":"http://127.0.0.1:NNNNN","hub_base":"http://127.0.0.1:MMMMM","token":"verify-token"}
//	# open hub_base in a browser, "+ Add box" with box_base + token.
//
// e2e_test.go is the automated counterpart: it starts the same stub
// in-process (via e2e.Start, no subprocess) and drives the hub page with
// Node + jsdom itself, so `go test ./tools/hub/e2e/...` (part of the
// standard `go test -race ./...`) exercises this without any manual step.
package main

import (
	"encoding/json"
	"os"

	"github.com/majorcontext/harness/tools/hub/e2e"
)

func main() {
	stub, err := e2e.Start()
	if err != nil {
		panic(err)
	}
	defer stub.Close()

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string{
		"box_base": stub.BoxBase,
		"hub_base": stub.HubBase,
		"token":    stub.Token,
	}); err != nil {
		panic(err)
	}
	select {} // block forever; kill the process to stop
}
