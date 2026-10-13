package plugin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/store"
)

var guard = &plugin.Plugin{
	Name: "guard",
	ToolExecuteBefore: &plugin.ToolBefore{
		DenyOnFailure: true,
		Run: func(ctx context.Context, c *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
			if req.Tool != "bash" {
				return nil, nil
			}
			var args struct{ Command string }
			if err := json.Unmarshal(req.Args, &args); err != nil {
				return nil, err
			}
			if strings.Contains(args.Command, "push --force") {
				return &plugin.ToolExecuteBeforeResponse{Deny: "force push is blocked"}, nil
			}
			return nil, nil
		},
	},
	HookTimeouts: map[plugin.Hook]time.Duration{plugin.HookToolExecuteBefore: time.Second},
}

// A plugin process is a main that calls Serve. The harness config names the
// program in a plugin entry whose name is the Plugin.Name.
func Example() {
	if err := plugin.Serve(guard); err != nil {
		log.Fatal(err)
	}
}

// A Go program that embeds Harness runs the same value in process.
func Example_inProcess() {
	rt, err := harness.New(harness.Options{
		Store:   store.NewMemStore(),
		WorkDir: ".",
		Getenv:  os.Getenv,
		Plugins: []*plugin.Plugin{guard},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt.Close(context.Background()) }()
}

// A plugin tool returns parts. A returned error becomes an error result for
// the model.
func ExampleTool() {
	_ = &plugin.Plugin{
		Name: "clock",
		Tools: []plugin.Tool{{
			Def: plugin.ToolDef{
				Name:        "now",
				Description: "Return the current UTC time.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			},
			Execute: func(ctx context.Context, c *plugin.Client, args json.RawMessage) ([]plugin.Part, error) {
				return []plugin.Part{plugin.TextPart(time.Now().UTC().Format(time.RFC3339))}, nil
			},
		}},
	}
}

func ExamplePlugin_Manifest() {
	b, _ := json.Marshal(guard.Manifest())
	fmt.Println(string(b))
	// Output: {"name":"guard","protocol_version":1,"hooks":["tool.execute.before"],"deny_on_failure":["tool.execute.before"],"hook_timeouts":{"tool.execute.before":1000}}
}
