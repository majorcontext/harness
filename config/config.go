// Package config is the harness runtime configuration: one JSON file with an
// optional project override. Loading reads at most two files, and starts no
// process and makes no network call.
package config

// Config is the parsed harness configuration. The zero value is valid and
// represents "no configuration": every method degrades to built-in defaults.
type Config struct {
	// Model is the default model ref ("provider/model") or an alias key.
	Model string `json:"model,omitempty"`
	// Aliases maps short names ("fast", "smart") to model refs. Resolution is
	// one level only — an alias target is never itself looked up as an alias.
	Aliases map[string]string `json:"aliases,omitempty"`
	// SessionDir is where session logs live. A leading "~/" is expanded
	// against $HOME at load time.
	SessionDir string `json:"session_dir,omitempty"`
	// Providers configures each provider family by name (e.g. "anthropic").
	Providers map[string]Provider `json:"providers,omitempty"`
	// Instructions, when set to false, disables project-instruction
	// (AGENTS.md) injection into the system prompt. A nil value (the field
	// omitted) leaves injection enabled — a *bool so "unset" and "false" are
	// distinguishable across the project-config merge.
	Instructions *bool `json:"instructions,omitempty"`
	// InstructionsPath overrides the auto-discovered AGENTS.md with a specific
	// file to load instead of walking up from the working directory.
	InstructionsPath string `json:"instructions_path,omitempty"`
	// InstructionsMaxBytes caps the bytes of each instruction file in the
	// system prompt. Zero: 64 KiB. Negative: no cap. A cut file gets a marker
	// and a WARN log line. HARNESS_INSTRUCTIONS_MAX_KB overrides it.
	InstructionsMaxBytes int `json:"instructions_max_bytes,omitempty"`
	// InstructionsMode "full" keeps a head and a marker for a file over the cap; else an outline follows the head.
	InstructionsMode string `json:"instructions_mode,omitempty"`
	// AppendSystemPrompt lists PLATFORM-owned facts the agent cannot
	// discover, and the platform policy that depends on them. Two hard
	// exclusions remain: project instructions belong in AGENTS.md, and tool
	// SHAPE (a schema, a description, when to call one server's tool) belongs
	// to the tool itself — an MCP server states its own usage through
	// initialize instructions, which the runtime renders as its own
	// segment. What is left for this key is the text no tool owns: what this
	// environment is, and what the platform running it guarantees.
	// The system prompt places entries after the base prompt and before
	// the AGENTS.md chain. Claude Code receives one blank-line-joined
	// --append-system-prompt-file value.
	//
	// Every segment must be BYTE-STABLE for the life of a session. These
	// entries sit at the front of the prompt-cache prefix, so a value that
	// varies per turn or per process start (a timestamp, a pod name, a live
	// status) re-processes the whole conversation uncached on every request,
	// with no error to notice. The runtime has no per-turn channel for configured
	// text, so a value that changes does not belong in this key.
	//
	// Merge is additive: base segments come first, then project segments.
	// This rule differs from every other slice field. In box deployments, the
	// base file belongs to the platform and the project file belongs to the
	// cloned repository. Override semantics would let the repository remove a
	// platform environment fact. Keep this field additive.
	AppendSystemPrompt []string `json:"append_system_prompt,omitempty"`
	// SkillsDirs lists the Agent Skills dirs. nil: <WorkDir>/.agents/skills. A
	// non-empty project value replaces the user value in the merge.
	SkillsDirs []string `json:"skills_dirs,omitempty"`
	// AgentDefsDirs lists the agent definition dirs of the runtime. nil: <WorkDir>/.agents.
	AgentDefsDirs []string `json:"agent_defs_dirs,omitempty"`
	// CommandsDirs lists directories scanned for prompt commands (*.md files).
	// A nil value uses <WorkDir>/.agents/commands. A non-empty project value
	// replaces the user value during config merge; explicit [] disables discovery.
	CommandsDirs []string `json:"commands_dirs,omitempty"`
	// GoalEvaluatorModel is the model ref or alias that judges goals. It has no
	// default: goals and the goal tool need it. Resolve it with ResolveModel.
	GoalEvaluatorModel string `json:"goal_evaluator_model,omitempty"`
	// MaxTaskDepth bounds the nesting of the child sessions of the task tool.
	MaxTaskDepth int `json:"max_task_depth,omitempty"`
	// MaxConcurrentTasks bounds the unsettled child sessions of one session tree.
	MaxConcurrentTasks int `json:"max_concurrent_tasks,omitempty"`
	// MaxTreeTokens stops a spawn once one session tree has used this many tokens. 0: no limit.
	MaxTreeTokens int `json:"max_tree_tokens,omitempty"`
	// ModelTool false disables the `model` session tool. nil leaves it
	// on, so a *bool keeps "unset" apart from "false" in the merge.
	ModelTool *bool `json:"model_tool,omitempty"`
	// Web opts in to the web tools. Each field is off when unset. A project
	// field overrides the user field; an unset one inherits. Fetch needs a WorkDir.
	Web WebSpec `json:"web,omitzero"`
	// Plugins lists the plugin processes to wire into every session's
	// turn hooks. Order matters: sync hooks chain across plugins in this
	// order, each seeing the previous plugin's mutations. A nil (omitted)
	// value disables plugins entirely. In the project-config merge the
	// project plugins come first and the user plugins last, so the most
	// trusted plugins see the final result. A name in both files is an error.
	Plugins []PluginSpec `json:"plugins,omitempty"`
	// PluginHTTPHeaders are stamped on every plugin's outbound HTTP traffic
	// (for example workspace attribution). Maps merge
	// key by key in the project-config merge, like Aliases and Providers.
	PluginHTTPHeaders map[string]string `json:"plugin_http_headers,omitempty"`
	// MCPServers declares named MCP servers the runtime connects to (lazily,
	// on first use) and registers as namespaced tools (mcp__<server>__<tool>,
	// the Claude Code convention). Keyed by server name;
	// a nil (omitted) value configures no MCP servers. In the project-config
	// merge, keys merge like Providers/Aliases (new keys from either layer
	// are kept) but a same-name project entry replaces the user entry
	// wholesale (see merge) — MCPServerSpec's Command/Env/Headers slices
	// make field-by-field merging (as Provider gets) more confusing than
	// useful here.
	MCPServers map[string]MCPServerSpec `json:"mcp_servers,omitempty"`
	// Processes declares named dev/support processes the runtime can
	// manage (start/stop/restart/status/logs) via the "process" session
	// tool and the /process endpoints. Keyed by process name; a nil (omitted) value
	// configures no processes. Merge rules mirror MCPServers: keys merge,
	// but a same-name project entry replaces the user entry wholesale.
	Processes map[string]ProcessSpec `json:"processes,omitempty"`
	// OwnerEpoch is the epoch of the grant of each session. 0 keeps epoch 1. cmd/harness reads it; harness.New does not.
	OwnerEpoch int `json:"owner_epoch,omitempty"`
	// PluginCache is the file that holds the probed manifest of each plugin. Only the user file sets it.
	PluginCache string `json:"plugin_cache,omitempty"`
	// CodexAuthFile is the file that holds the Codex login of `harness login codex`. Only the user file sets it. cmd/harness defaults it to auth/codex.json beside $HARNESS_CONFIG, else in ~/.harness.
	CodexAuthFile string `json:"codex_auth_file,omitempty"`
	// CodexIssuer is the base URL of the OAuth issuer of the Codex login, https://auth.openai.com when empty.
	// Only the user file sets it: no HARNESS_ variable does, so Validate sees the value that is used.
	CodexIssuer string `json:"codex_issuer,omitempty" env:"-"`
	// Sync posts each record to a control plane. Only the user file sets it. cmd/harness reads it; harness.New does not.
	Sync *SyncSpec `json:"sync,omitempty"`
	// ContextWindowTokens is the context window of every model, in tokens,
	// and replaces the built-in table (package modelmeta, sourced from
	// models.dev). When it is zero, a model that the table lacks runs with
	// a default of 128000 tokens, and its gauge marks the window as an
	// estimate. A context overflow error never changes the window.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	// RequestMaxRetries sets how many times one model API request is sent
	// again after a 5xx status or a transport error, before any response
	// byte streams. A 429 is not sent again by the request. A nil value
	// leaves the default of 4; an explicit 0 turns the retry off. Resolve it
	// with RequestMaxRetriesValue.
	RequestMaxRetries *int `json:"request_max_retries,omitempty"`
	// StreamMaxRetries sets how many times a turn calls the model again after a
	// retryable error of a call that recorded no item: a request that failed
	// after its own retries, a 429, a stream that ended early, a content
	// filter. A nil value leaves the default of 5; an explicit 0 turns the
	// retry off. Resolve it with StreamMaxRetriesValue.
	StreamMaxRetries *int `json:"stream_max_retries,omitempty"`
	// MaxTokens caps the output tokens of each model call of a turn. Zero is the backend default.
	// A negative value fails Validate.
	MaxTokens int `json:"max_tokens,omitempty"`
	// StreamIdleTimeoutS sets the stream idle timeout (in seconds)
	// of every session this process creates: how long a streamed response
	// may go without a delta before the idle-stream watchdog
	// aborts it. Zero (omitted, the default) leaves the 5-minute
	// default in place;
	// negative disables the watchdog entirely.
	StreamIdleTimeoutS int `json:"stream_idle_timeout_s,omitempty"`
	// CompactionThreshold sets the
	// fraction of ContextWindowTokens at which automatic compaction
	// triggers. Zero (omitted) defaults to 0.8 (see Defaults).
	CompactionThreshold float64 `json:"compaction_threshold,omitempty"`
	// CompactionKeepTurns sets how many
	// of the most recent turns automatic compaction always keeps verbatim.
	// Zero (omitted) defaults to 2 (see Defaults); the effective value
	// can never go below 1.
	CompactionKeepTurns int `json:"compaction_keep_turns,omitempty"`
	// MCPToolLoading selects when a session defers MCP tool SCHEMAS instead
	// of registering every one of them up front (see
	// docs/design/mcp-lazy-tools.md). "eager" (the default, and what an
	// absent value means) keeps today's behaviour: every tool of every
	// connected server registers with its full JSON Schema. "lazy" defers
	// every server: its tools appear as a name-only catalog in the system
	// prompt, and the model loads the schema it needs with the mcp tool's
	// select action. "auto" defers only once the live catalog holds more
	// tools than MCPToolLoadingThreshold. An unrecognized value is rejected
	// by validateMCPToolLoading rather than falling back to the default:
	// deferral changes what the model can call without asking, so a typo
	// must never decide it silently.
	MCPToolLoading string `json:"mcp_tool_loading,omitempty"`
	// MCPToolLoadingThreshold is the tool COUNT "auto" compares the live
	// catalog against. It is consulted in "auto" mode ONLY: with "eager" or
	// "lazy" the decision needs no catalog size, so a threshold set beside
	// either of them is accepted and inert. It is not rejected there,
	// because a config that switches mode back to "auto" should not have to
	// re-supply it; 0/absent leaves the threshold at its default of 20
	// (see Defaults). A count rather than a token estimate: the
	// runtime has no tokenizer on the request path. A NEGATIVE value cannot
	// possibly be wired -- len(catalog) > -1 holds even for an empty
	// catalog, so a stray minus sign would silently turn "auto" into "always
	// defer" -- and is rejected loudly, like MCPServerSpec.ConnectTimeoutS.
	MCPToolLoadingThreshold int `json:"mcp_tool_loading_threshold,omitempty"`
	// SubscriptionDefault lets an unset model resolve to the subscription of the user (see defaultModel). Only the
	// composition root of a command that a person runs sets it. No file, variable, or flag does.
	SubscriptionDefault bool `json:"-"`
}

// WebSpec holds the switches of the web tools. A nil field is off, and a
// *bool keeps "unset" apart from "false" across the project-config merge.
type WebSpec struct {
	// Fetch offers the web_fetch tool.
	Fetch *bool `json:"fetch,omitempty"`
	// Search offers the native web search of the provider of the model, where the provider has one.
	Search *bool `json:"search,omitempty"`
}

// ResolveModel returns s, else Model, else the default model (see defaultModel), after one alias lookup. It does not parse.
func (c *Config) ResolveModel(s string) string {
	if s == "" && c != nil {
		s = c.Model
	}
	if s == "" {
		s = c.defaultModel()
	}
	if c != nil {
		if target, ok := c.Aliases[s]; ok {
			s = target
		}
	}
	return s
}
