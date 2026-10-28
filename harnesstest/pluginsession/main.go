// Command pluginsession serves one plugin tool, who, that returns the session
// ID from plugin.SessionID. It is a process plugin written with the Go API.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/majorcontext/harness/plugin"
)

func main() {
	p := &plugin.Plugin{Name: "whoproc", Tools: []plugin.Tool{{
		Def: plugin.ToolDef{Name: "who_proc", Description: "reports the session", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Execute: func(ctx context.Context, _ *plugin.Client, _ json.RawMessage) ([]plugin.Part, error) {
			return []plugin.Part{plugin.TextPart("session=" + plugin.SessionID(ctx))}, nil
		}}}}
	if err := plugin.Serve(p); err != nil {
		fmt.Fprintln(os.Stderr, "pluginsession:", err)
		os.Exit(1)
	}
}
