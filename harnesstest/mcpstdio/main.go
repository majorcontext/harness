// Command mcpstdio serves the harnesstest.MCPSpec in $HARNESSTEST_MCP_SPEC
// over stdio, as a scripted MCP server for e2e tests.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/majorcontext/harness/harnesstest"
)

func main() {
	var spec harnesstest.MCPSpec
	if err := json.Unmarshal([]byte(os.Getenv(harnesstest.MCPSpecEnv)), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "mcpstdio: bad spec:", err)
		os.Exit(2)
	}
	if err := harnesstest.ServeMCPStdio(os.Stdin, os.Stdout, spec); err != nil {
		fmt.Fprintln(os.Stderr, "mcpstdio:", err)
		os.Exit(1)
	}
}
