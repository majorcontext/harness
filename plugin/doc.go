// Package plugin is the Go API for Harness plugins.
//
// A plugin changes what a Harness session does at fixed points, the hooks,
// and can add tools for the model. For example, a tool.execute.before hook can
// deny a dangerous shell command before it runs, and a system.transform hook
// can add text to the system prompt.
//
// # Write a plugin
//
// A plugin is a *[Plugin] value. Set the hook fields that the plugin needs and
// leave the others nil. A nil hook field is not subscribed. [Plugin.Manifest] derives
// the hook and tool list from the fields that are set, so the manifest always
// agrees with the code.
//
//	var guard = &plugin.Plugin{
//		Name: "guard",
//		ToolExecuteBefore: &plugin.ToolBefore{
//			DenyOnFailure: true,
//			Run: func(ctx context.Context, c *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
//				if req.Tool != "bash" {
//					return nil, nil // no change
//				}
//				var args struct {
//					Command string `json:"command"`
//				}
//				if err := json.Unmarshal(req.Args, &args); err != nil {
//					return nil, err
//				}
//				if strings.Contains(args.Command, "push --force") {
//					return &plugin.ToolExecuteBeforeResponse{Deny: "force push is blocked"}, nil
//				}
//				return nil, nil
//			},
//		},
//	}
//
// # Run a plugin
//
// One value runs in two ways, with the same chain order, deadlines, and
// failure rules:
//
//   - In process. A Go program that embeds Harness puts the value in
//     harness.Options.Plugins. The runtime serves it over an in-memory pipe.
//   - As a process. A small main calls [Serve]. The plugin entry of the user or
//     project config names the program in "command", and its "name" must equal
//     [Plugin].Name. The host runs the program and speaks JSON-RPC to it on
//     standard input and output. Standard output is the protocol channel, so
//     write logs to standard error. Run "harness plugin probe" to check the
//     manifest. The probe sends initialize with only the protocol version and
//     the config, so the manifest must not depend on other values.
//
// A process plugin entry looks like this:
//
//	{"plugins": [{"name": "guard", "command": ["/usr/local/bin/guard-plugin"]}]}
//
// # Hooks
//
//   - ToolExecuteBefore runs before each tool call that the harness runs:
//     built-in, MCP, embedder, and plugin tools. Return Args to replace the arguments for the rest of
//     the chain, or Deny to stop the chain. A denied call does not run, and
//     the model gets the Deny text as an error result. The session log keeps
//     the call as the model made it.
//   - ToolExecuteAfter runs after each tool call that ran, unless the turn
//     stopped. Output holds one
//     text part with the result text. Return Output to replace that text: the
//     host joins its text parts with a newline. The error flag and the blobs
//     of the result do not change.
//   - ChatParams runs before each model call. Params replaces all the
//     parameters of the call, so start from a copy of the request Params.
//     Model takes a provider/model reference, not an alias; the host skips a
//     response with an alias. A model of another kind of backend fails the
//     turn.
//   - SystemTransform runs before each model call. The host adds the segments
//     of each plugin to the end of the system prompt, in chain order.
//   - ShellEnv runs before each bash command. The host adds the variables to
//     the command environment, in chain order, so a later plugin wins.
//   - Event gets the runtime events in batches. The host sends each batch to
//     each subscribed plugin, not as a chain, and does not wait for it.
//   - ChatMessage is part of the protocol, but the runtime never calls it.
//
// A backend that runs its own agent loop, such as Claude Code, calls no
// ChatParams, SystemTransform, or ShellEnv hook. It runs its own shell and file
// tools and connects its own MCP servers, and those calls run no hook. Only its
// calls to embedder and plugin tools run the tool hooks. A guard does not see
// a Claude Code shell command.
//
// # Events
//
// A turn that starts or resumes sends session.status busy, and a turn that
// ends or suspends sends session.status idle. A failed turn sends session.error before its idle
// status. A tool call that runs sends tool.execute.start and
// tool.execute.end; a denied call sends neither. A write_file or edit_file
// call that succeeds sends file.edited with an absolute path. The runtime does
// not send question.asked.
//
// # Chain order
//
// Each hook except Event runs as a chain over all subscribed plugins. The order is the
// project config plugins, then the user config plugins, then the plugins of
// harness.Options.Plugins in list order. The most trusted plugins run last, so
// a user guard sees the arguments after each project rewrite.
//
// # Failure
//
// A hook chain fails open by default. When a hook returns an error, panics,
// misses its deadline, sends a response that does not decode, or its plugin
// does not start or exits, the host skips that plugin and continues the chain.
// For a guard, set [ToolBefore].DenyOnFailure: then a failure stops the chain,
// the tool does not run, and the model gets the error result
// "denied: plugin <name> failed: <error>". A plugin that fails to start stays
// failed until the runtime closes, so a guard that cannot start denies every
// call of its hook. A stopped turn is not a failure: it denies nothing.
//
// # Deadlines and contexts
//
// Each hook except Event has a deadline: the [Plugin].HookTimeouts value for
// that hook, or 5 s. The ctx of the hook ends at the deadline, and the host
// stops waiting at that deadline. The ctx of an Event call and of a tool call
// has no deadline; it ends when the connection ends. The protocol has no
// cancel message: when a turn stops, the host stops waiting, but a tool
// call keeps running until it returns or the connection ends.
//
// The host overlaps calls, and one *Plugin can serve several runtimes. Every
// hook and tool function must be safe for concurrent use.
//
// # Call the host
//
// Each hook and tool gets a *[Client]. Use it to read the plugin config, read
// the messages of a session, call a tool of an MCP server of the harness
// config, or make one model call. These calls do not run the hook chain.
//
// # Wire records
//
// Some exported types are records of the wire protocol, such as
// [InitializeParams], [EventBatch], [ToolExecuteRequest], and [MCPCallRequest]. Serve
// and the host use them. A plugin does not need them. The protocol is in
// https://github.com/majorcontext/harness/blob/main/internal/plugin/PROTOCOL.md.
//
// The guardrail example in
// https://github.com/majorcontext/harness/tree/main/examples/guardrail runs one
// guard value in both ways.
package plugin
