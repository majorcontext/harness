// Package config loads the harness CLI configuration: a single JSON file
// (plus an optional per-project override) parsed in one flat pass with the
// standard library only. Nothing here touches the network or spawns
// processes — config loading sits on the startup path, so it is at most two
// file reads (the user config, plus the project override when present).
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
	// initialize instructions, which internal/tool/mcpsrc renders as its own
	// segment. What is left for this key is the text no tool owns: what this
	// environment is, and what the platform running it guarantees.
	// internal/prompt.Build places entries after the base prompt and before
	// the AGENTS.md chain. Claude Code receives one blank-line-joined
	// --append-system-prompt value.
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
	// Plugins lists the plugin processes to wire into every session's
	// turn hooks (see internal/tool/pluginsrc). Order matters: sync hooks
	// chain across plugins in this order, each seeing the previous plugin's
	// mutations. A nil (omitted) value disables plugins entirely. In the
	// project-config merge a non-empty project value replaces the user
	// value entirely (arrays override, like SkillsDirs).
	Plugins []PluginSpec `json:"plugins,omitempty"`
	// PluginHTTPHeaders are stamped on every plugin's outbound HTTP traffic
	// (plugin.Options.HTTPHeaders, e.g. workspace attribution). Maps merge
	// key by key in the project-config merge, like Aliases and Providers.
	PluginHTTPHeaders map[string]string `json:"plugin_http_headers,omitempty"`
	// MCPServers declares named MCP servers the runtime connects to (lazily,
	// on first use) and registers as namespaced tools (mcp__<server>__<tool>,
	// the Claude Code convention — see internal/tool/mcpsrc). Keyed by server name;
	// a nil (omitted) value configures no MCP servers. In the project-config
	// merge, keys merge like Providers/Aliases (new keys from either layer
	// are kept) but a same-name project entry replaces the user entry
	// wholesale (see merge) — MCPServerSpec's Command/Env/Headers slices
	// make field-by-field merging (as Provider gets) more confusing than
	// useful here.
	MCPServers map[string]MCPServerSpec `json:"mcp_servers,omitempty"`
	// Processes declares named dev/support processes the runtime can
	// manage (start/stop/restart/status/logs) via the "process" session
	// tool and the /process endpoints (see internal/tool/proc). Keyed by process name; a nil (omitted) value
	// configures no processes. Merge rules mirror MCPServers: keys merge,
	// but a same-name project entry replaces the user entry wholesale.
	Processes map[string]ProcessSpec `json:"processes,omitempty"`
	// OwnerEpoch is the epoch of the grant of each session. 0 keeps epoch 1.
	OwnerEpoch int `json:"owner_epoch,omitempty"`
	// PluginCache is the file that holds the probed manifest of each plugin. Only the user file sets it.
	PluginCache string `json:"plugin_cache,omitempty"`
	// Sync posts each record to a control plane. Only the user file sets it.
	Sync *SyncSpec `json:"sync,omitempty"`
	// ContextWindowTokens is the context window of every model, in tokens,
	// and replaces the built-in table (package modelmeta, sourced from
	// models.dev). When it is zero, a model that the table lacks runs with
	// a default of 128000 tokens, and its gauge marks the window as an
	// estimate. A context overflow error never changes the window.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	// PromptRetries sets how many ADDITIONAL
	// attempts a turn (internal/turn) makes when a model call fails
	// with a transient, retryable provider error (an HTTP 5xx/429/529 or a
	// truncated stream). A nil value (the field omitted) leaves the product
	// default of 2 in place; an explicit 0 disables the retry entirely. A
	// *int distinguishes "unset" (2) from "0" (off) across the project-config
	// merge, exactly like ModelTool above. Resolve it with
	// PromptRetriesValue. It is deliberately small and short.
	PromptRetries *int `json:"prompt_retries,omitempty"`
	// MaxTokensContinuations sets how many CONSECUTIVE times a
	// turn (internal/turn) auto-continues
	// a turn that stopped with provider reason "max_tokens" (the provider
	// cut the model off mid-emission) instead of settling the turn. A nil
	// value (the field omitted) leaves the
	// product default of 3 in place; an explicit 0 disables auto-continue
	// entirely, reverting to the pre-fix behavior. A *int distinguishes
	// "unset" (3) from "0" (off) across the project-config merge, exactly
	// like PromptRetries above. Resolve it with
	// MaxTokensContinuationsValue.
	MaxTokensContinuations *int `json:"max_tokens_continuations,omitempty"`
	// StreamIdleTimeoutS sets the stream idle timeout (in seconds)
	// of every session this process creates: how long a streamed response
	// may go without a delta before the idle-stream watchdog of internal/turn
	// aborts it. Zero (omitted, the default) leaves the 5-minute
	// default in place (mirroring Codex's stream_idle_timeout_ms=300000);
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
	// SessionSync selects the durability mechanism for attested session-store
	// writes (durable enqueue, session-create persist). "fsync" (default)
	// fsyncs the log file and, on first creation, its directory — correct for
	// local POSIX filesystems. "volume" skips both fsync round-trips: for
	// stores on continuously-synced network volumes whose own commit layer is
	// the documented durability boundary, where fsync adds no durability and
	// some FUSE/9p transports deadlock permanently on it (fsync(dirfd)
	// especially). With "volume", an attestation means the write(2) completed
	// and durability is delegated to the volume layer.
	SessionSync string `json:"session_sync,omitempty"`
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
}

// ResolveModel returns s, else Model, else DefaultModel, after one alias lookup. It does not parse.
func (c *Config) ResolveModel(s string) string {
	if s == "" && c != nil {
		s = c.Model
	}
	if s == "" {
		s = DefaultModel
	}
	if c != nil {
		if target, ok := c.Aliases[s]; ok {
			s = target
		}
	}
	return s
}
