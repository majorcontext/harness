# Plugins

A plugin runs before or after each tool call, and can add tools and call back
into the host. This page shows a plugin as a process and a plugin in the
process of your program. For the lifecycle and the wire contract, see
[plugins-and-protocols.md](plugins-and-protocols.md).

## Block tool calls with a plugin

A plugin runs before each tool call and can deny it. This one is a Go program
that blocks `rm` in shell commands. Make a module for it:

```bash
mkdir guard-plugin && cd guard-plugin
go mod init example.com/guard-plugin
go get github.com/majorcontext/harness@latest
```

Write `main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/majorcontext/harness/plugin"
)

var guard = &plugin.Plugin{
	Name: "guard",
	ToolExecuteBefore: &plugin.ToolBefore{
		DenyOnFailure: true,
		Run:           check,
	},
}

func check(ctx context.Context, c *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
	if req.Tool != "bash" {
		return nil, nil
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(req.Args, &args); err != nil {
		return nil, err
	}
	for _, word := range strings.Fields(args.Command) {
		if word == "rm" {
			return &plugin.ToolExecuteBeforeResponse{Deny: "rm is blocked"}, nil
		}
	}
	return nil, nil
}

func main() {
	if err := plugin.Serve(guard); err != nil {
		log.Fatal(err)
	}
}
```

`Run` returns nil to let the call run, or a `Deny` message, which the model
gets as the error result of the call. With `DenyOnFailure`, a hook that
returns an error, panics, or misses its deadline also denies the call, so a
broken guard does not let a call through. `plugin.Serve` speaks the protocol
on standard input and output, so the plugin must log only to standard error.

Install it. The binary takes the last element of the module path,
`guard-plugin`:

```bash
go install .
```

Add it to the `plugins` key of `.harness.json` in your project. `name` must
equal `Plugin.Name`, and `command` is found on your `PATH`:

```json
{"plugins": [{"name": "guard", "command": ["guard-plugin"]}]}
```

A call to a plugin tool ends after 120 seconds. Set `tool_timeout_s` on the plugin entry to change it.

A plugin tool can read the ID of the session that called it. In `Tool.Execute`, call `plugin.SessionID(ctx)`. It works for a process plugin and for an in-process plugin, and returns an empty string for a context that no tool call made.

Check that Harness finds it. The probe prints each plugin and its hooks:

```console
$ harness plugin probe
guard: tool.execute.before
```

Then ask for a call it blocks. The tool lines show the call and the deny
message:

```console
$ echo scratch > scratch.txt
$ harness run -p "Delete scratch.txt with the bash tool"
[tool bash] {"command":"rm scratch.txt"}
[tool bash failed] rm is blocked
```

The guard reads the text of a command. It is not a sandbox: the model can try
another command to get the same result, such as a Python script that deletes
the file. After a rebuild, the next run or probe sees the new binary. A plugin can also
change requests and results, add tools, and call back into the host. A plugin
in another language speaks the same protocol over stdio. See the
[`plugin` package reference](https://pkg.go.dev/github.com/majorcontext/harness/plugin),
the [TypeScript SDK](../sdk/typescript/README.md), the
[protocol](../internal/plugin/PROTOCOL.md), and
[`examples/guardrail`](../examples/guardrail).

## Run a plugin in the process

The `guard` value of [the walkthrough above](#block-tool-calls-with-a-plugin) also
runs inside your program, with no separate process and no config entry. Copy
its `guard` variable and `check` function into your program, add the imports
`strings` and `github.com/majorcontext/harness/plugin`, and pass it to the
runtime:

```go
rt, err := harness.New(harness.Options{
	Store:   store.NewMemStore(),
	WorkDir: ".",
	Getenv:  os.Getenv,
	Plugins: []*plugin.Plugin{guard},
})
```

The runtime applies the same hook order, deadlines, and `DenyOnFailure` rule
as for a process plugin. It runs config plugins first and the plugins of
`Options.Plugins` last.
