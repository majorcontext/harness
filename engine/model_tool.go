// Model self-switch: the `model` session tool lets the model itself inspect
// and swap this session's MAIN model from inside a running turn, in-process —
// no HTTP round-trip. The heavy machinery already exists: Session.SetModel
// swaps the model, persists the durable recModel resume record, and emits
// EventModelChanged; a per-turn transcode rebuilds the request for whichever
// model is current (see engine.go). This tool is the model-facing surface over
// that machinery.
//
// Three actions: status (read-only) reports the current model, the
// configured aliases, and the configured provider names; list (read-only) is
// status's data minus the current model, for a caller that only wants the
// choices — e.g. a delegated caller picking a family for another tool's own
// model override (task's spawn action) rather than swapping THIS session's
// model; set(model) resolves a one-level alias, validates the target
// provider is configured, and calls SetModel. There is deliberately no clear
// action — a model always has a model; there is nothing to clear.
//
// Gated by Config.ModelTool: registered in newSession only when the host opts
// in. Unlike GoalTool (opt-in, gated on a configured evaluator), the CLI/server
// wiring sets ModelTool true by default (config key `model_tool`), so an
// operator opts OUT.
//
// Scope: the MAIN session model only. This never touches the goal-evaluator or
// any subagent model.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// modelToolName is the session tool's fixed name.
const modelToolName = "model"

// ModelToolName exports modelToolName for a caller outside this package
// that needs to name the SAME tool RunTool/ToolDef dispatch by —
// server/mcp_history.go's harness-hosted MCP `model` tool entry, notably —
// without hand-duplicating the literal "model" and risking it silently
// drifting from this package's own internal name. Mirrors
// engine/process.go's identical ProcessToolName export.
const ModelToolName = modelToolName

// modelToolArgs is the model tool's input shape.
type modelToolArgs struct {
	Action string `json:"action"`
	Model  string `json:"model"`
}

// Provider billing classifications reported on every model tool
// list/status providers[] entry — see billingForProvider. Two values only:
// a configured provider is either paid for by a running subscription
// (billingSubscription) or billed per API call (billingAPI). Nothing in
// this codebase configures a provider harness cannot place in one of
// those two buckets (see billingForProvider's doc comment), so a third
// value is deliberately absent rather than spelled out unused.
const (
	billingSubscription = "subscription"
	billingAPI          = "api"
)

// codexProviderFamily mirrors provider/openai.CodexFamily's conventional
// providers-map key: an entry named "codex" speaks the ChatGPT Codex
// backend and is billed against the operator's ChatGPT subscription
// (provider/openai captures its x-codex-* subscription-usage response
// headers only for a client whose resolved family equals this string).
// Duplicated here, like ClaudeCodeProviderFamily is duplicated in
// provider/claudecode, rather than imported: this package must not import
// a concrete provider adapter package for one string.
// TestCodexProviderFamilyMatchesOpenAIPackage pins the two from drifting
// apart.
const codexProviderFamily = "codex"

// billingForProvider classifies name — a configured provider's registry
// key, i.e. a message.ModelRef.Provider value — as subscription-backed or
// API-billed, for the model tool's list/status output (see
// providerInfo). It is a pure display classification of the two
// conventions harness already treats as structurally distinct elsewhere:
// ClaudeCodeProviderFamily (every turn delegates to the locally
// subscription-authenticated `claude` CLI, never an API key — see
// engine/claude_code_backend.go) and codexProviderFamily (speaks the
// ChatGPT Codex backend and reports subscription-usage headers, see
// provider/openai.CodexFamily). Every other configured provider —
// the native anthropic/openai adapters, any openai-compat entry, and any
// openai entry not named "codex" by convention (e.g. a deployment's own
// "bifrost" gateway key) — is an HTTP adapter authenticated with an API
// key or a deployment-provided base URL, so it classifies as billingAPI.
//
// This adds no new naming rule: it surfaces the same "the operator's own
// key IS the signal" convention CodexFamily's own doc comment already
// documents, purely as a response field an agent can read instead of
// needing prior, out-of-band knowledge of this platform's naming
// convention.
func billingForProvider(name string) string {
	switch name {
	case ClaudeCodeProviderFamily, codexProviderFamily:
		return billingSubscription
	default:
		return billingAPI
	}
}

// providerInfo is one configured provider's registry name plus its
// billing classification (see billingForProvider) — every model tool
// list/status providers[] entry, so an agent told to prefer a
// subscription-backed model (see docs/models-and-providers.md) can act on
// that instruction from this tool's own response.
type providerInfo struct {
	Name    string `json:"name"`
	Billing string `json:"billing"`
}

// modelToolResult is the JSON payload every model tool action returns: the
// current model plus the configured aliases and provider names, so the model
// can pick a valid target from one status call. Aliases and Providers are
// sorted for deterministic output.
type modelToolResult struct {
	Model     string            `json:"model"`
	Aliases   map[string]string `json:"aliases,omitempty"`
	Providers []providerInfo    `json:"providers,omitempty"`
}

// modelListResult is the list action's return: the configured provider
// families and aliases a caller can spawn or set into, WITHOUT this
// session's current model — a delegated caller (e.g. a claude-code-lane
// agent reaching this tool through the harness-hosted MCP shim,
// server/mcp_history.go) asking "what models are available" is not asking
// about this particular session's own state, unlike status. Backed by the
// exact same configuredProviderInfos()/ModelAliases data modelToolStatus
// reads (see modelToolList) — never a second, independent data source
// that could drift from it.
type modelListResult struct {
	Providers []providerInfo    `json:"providers"`
	Aliases   map[string]string `json:"aliases,omitempty"`
}

// modelTool builds the `model` session tool. See the package doc for the
// action contract.
func modelTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name: modelToolName,
			Description: "Inspect or swap this session's MAIN model. History transcodes " +
				"automatically for whichever model is current — there is no migration step. " +
				"Actions: " +
				"status() reports the current model, the configured aliases, and the configured " +
				"providers, each with a \"billing\" of \"subscription\" (paid for by a running " +
				"subscription) or \"api\" (billed per call); " +
				"set(model) swaps the main model to a full \"provider/model\" ref or a configured " +
				"alias — it takes effect on the NEXT request in this session. set fails, and " +
				"changes nothing, if the target names an unconfigured provider (the error lists the " +
				"valid aliases and provider names). " +
				"list() reports the same configured providers (with billing) and aliases, with no " +
				"current-model or session state — useful to pick a family/model for another tool's " +
				"own model override (e.g. task's spawn action) without swapping this session's own model. " +
				"There is no action to clear the model — a session always has a model.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "enum": ["status", "set", "list"], "description": "The operation to perform"},
					"model": {"type": "string", "description": "The target model: a \"provider/model\" ref or a configured alias (required for set)"}
				},
				"required": ["action"]
			}`),
		},
		// Serial: set swaps s.model via SetModel, which every later call in
		// the batch (and every later request) must see consistently. A
		// barrier keeps a sibling call from running against a model that
		// is about to change mid-batch.
		Serial: true,
		Run: func(_ context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			return runModelTool(s, args)
		},
	}
}

// runModelTool dispatches one model tool call against s.
func runModelTool(s *Session, raw json.RawMessage) (message.Parts, error) {
	var in modelToolArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("model: invalid arguments: %w", err)
	}

	switch in.Action {
	case "status":
		return jsonResult(s.modelToolStatus())

	case "list":
		return jsonResult(s.modelToolList())

	case "set":
		if in.Model == "" {
			return nil, fmt.Errorf("model: set requires a non-empty model (%s)", s.modelChoicesHint())
		}
		// Resolve a one-level alias, matching config.ResolveModel's alias step
		// (the engine never imports config, so ModelAliases mirrors it). An
		// alias target is never itself looked up as an alias.
		ref, err := s.resolveModelRef(in.Model)
		if err != nil {
			return nil, fmt.Errorf("model: %w (%s)", err, s.modelChoicesHint())
		}
		// Validate the provider is configured BEFORE swapping: a set to an
		// unconfigured provider must change nothing, so an unusable ref never
		// wedges every later request. ModelSupported is the ONE provider-
		// configured check both this tool and the POST /session/{id}/model
		// endpoint share, so the two never drift.
		if !s.ModelSupported(ref) {
			return nil, fmt.Errorf("model: provider %q is not configured (%s)", ref.Provider, s.modelChoicesHint())
		}
		// And that a context window is known for it, for the same
		// before-the-swap reason: CheckModel is the sibling gate every
		// SetModel route shares (see Config.RequireContextWindow), so a
		// model with no known window is refused here instead of silently
		// leaving the session with no context management.
		if err := s.CheckModel(ref); err != nil {
			return nil, fmt.Errorf("model: %w", err)
		}
		s.SetModel(ref)
		return jsonResult(s.modelToolStatus())

	default:
		return nil, fmt.Errorf("model: unknown action %q (valid actions: status, set, list — there is no clear action)", in.Action)
	}
}

// resolveModelRef resolves in through a one-level alias lookup against
// ModelAliases, then parses it as a "provider/model" ref. It replicates
// config.ResolveModel's alias step so the engine need not import config.
func (s *Session) resolveModelRef(in string) (message.ModelRef, error) {
	if target, ok := s.cfg.ModelAliases[in]; ok {
		in = target
	}
	return message.ParseModelRef(in)
}

// modelToolStatus builds the current model status: the current model plus the
// sorted configured aliases and provider names.
func (s *Session) modelToolStatus() modelToolResult {
	res := modelToolResult{
		Model:     s.Model().String(),
		Providers: s.configuredProviderInfos(),
	}
	if len(s.cfg.ModelAliases) > 0 {
		aliases := make(map[string]string, len(s.cfg.ModelAliases))
		for k, v := range s.cfg.ModelAliases {
			aliases[k] = v
		}
		res.Aliases = aliases
	}
	return res
}

// modelToolList builds the list action's result: the configured provider
// families and aliases only — the same underlying data modelToolStatus
// reads (configuredProviderInfos/ModelAliases), just without the current
// model field a bare "what models are available" query has no use for.
func (s *Session) modelToolList() modelListResult {
	res := modelListResult{Providers: s.configuredProviderInfos()}
	if len(s.cfg.ModelAliases) > 0 {
		aliases := make(map[string]string, len(s.cfg.ModelAliases))
		for k, v := range s.cfg.ModelAliases {
			aliases[k] = v
		}
		res.Aliases = aliases
	}
	return res
}

// configuredProviderNames returns the configured provider names, sorted.
func (s *Session) configuredProviderNames() []string {
	if len(s.cfg.Providers) == 0 {
		return nil
	}
	names := make([]string, 0, len(s.cfg.Providers))
	for name := range s.cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// configuredProviderInfos returns the configured providers, sorted by
// name, each paired with its billing classification (see
// billingForProvider) — the data source both modelToolStatus and
// modelToolList's providers[] field share, so list and status can never
// report divergent billing for the same provider.
func (s *Session) configuredProviderInfos() []providerInfo {
	names := s.configuredProviderNames()
	if len(names) == 0 {
		return nil
	}
	infos := make([]providerInfo, len(names))
	for i, name := range names {
		infos[i] = providerInfo{Name: name, Billing: billingForProvider(name)}
	}
	return infos
}

// modelChoicesHint renders the valid aliases and provider names for a set
// error, so a rejected set tells the model what it CAN switch to.
func (s *Session) modelChoicesHint() string {
	aliases := make([]string, 0, len(s.cfg.ModelAliases))
	for name := range s.cfg.ModelAliases {
		aliases = append(aliases, name)
	}
	sort.Strings(aliases)
	provs := s.configuredProviderNames()
	return fmt.Sprintf("valid aliases: %v; configured providers: %v", aliases, provs)
}
