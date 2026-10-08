package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const modelToolName = "model"

const modelDescription = "Inspect or swap this session's MAIN model. History transcodes " +
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
	"There is no action to clear the model — a session always has a model."

const modelSchema = `{
	"type": "object",
	"properties": {
		"action": {"type": "string", "enum": ["status", "set", "list"], "description": "The operation to perform"},
		"model": {"type": "string", "description": "The target model: a \"provider/model\" ref or a configured alias (required for set)"}
	},
	"required": ["action"]
}`

const modelListDescription = "List the provider families and aliases configured on this box, each provider tagged with a \"billing\" of \"subscription\" or \"api\". " +
	"Use this to pick a family for task's own spawn(model:...) override when delegating to a child session. " +
	"This surface exposes ONLY the list action — inspecting or changing THIS session's own current model is not available here; " +
	"task's model override is the way to select a model, for a CHILD session, not this one."

const modelListSchema = `{
	"type": "object",
	"properties": {
		"action": {"type": "string", "enum": ["list"], "description": "The operation to perform; only \"list\" is exposed on this surface"}
	},
	"required": ["action"]
}`

const (
	billingSubscription = "subscription"
	billingAPI          = "api"
	claudeCodeFamily    = "claude-code"
	codexFamily         = "codex"
)

type providerInfo struct {
	Name    string `json:"name"`
	Billing string `json:"billing"`
}

// billing classifies a configured provider: the claude-code and codex
// families run on a subscription, every other provider bills each API call.
func billing(provider string) string {
	if provider == claudeCodeFamily || provider == codexFamily {
		return billingSubscription
	}
	return billingAPI
}

type modelStatus struct {
	Model     string            `json:"model"`
	Aliases   map[string]string `json:"aliases,omitempty"`
	Providers []providerInfo    `json:"providers,omitempty"`
}

type modelList struct {
	Providers []providerInfo    `json:"providers"`
	Aliases   map[string]string `json:"aliases,omitempty"`
}

// modelTool lets the model inspect and swap the model of its session. The
// runtime binds session when the session starts. A backend that owns its
// loop gets the list action alone, because set would move the model of the
// turn that calls it.
type modelTool struct {
	r        *Runtime
	session  string
	listOnly bool
}

func (t modelTool) Spec() protocol.ToolSpec {
	if t.listOnly {
		return protocol.ToolSpec{Name: modelToolName, Description: modelListDescription, InputSchema: json.RawMessage(modelListSchema)}
	}
	return protocol.ToolSpec{Name: modelToolName, Description: modelDescription, InputSchema: json.RawMessage(modelSchema)}
}

// Bind returns the tool of session id.
func (t modelTool) Bind(id string, _ bool) turn.Tool {
	t.session = id
	return t
}

// Alone makes the model tool run with no other call of its model call in flight.
func (modelTool) Alone() {}

func (t modelTool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Action, Model string }
	if err := json.Unmarshal(c.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("model: invalid arguments: %w", err)
	}
	s := t.r.running(t.session)
	if s == nil {
		return protocol.ToolResult{}, ErrSessionNotOwned
	}
	switch {
	case in.Action == "list":
		return jsonResult(modelList{Providers: t.r.providerInfos(), Aliases: t.r.aliasCopy()})
	case t.listOnly:
		return protocol.ToolResult{}, fmt.Errorf("model: action %q is not available on this surface (only \"list\" is exposed here — use task's own model override to select a model for a child session)", in.Action)
	case in.Action == "status":
		return jsonResult(t.r.modelStatus(s.View().Model))
	case in.Action == "set":
		v, err := t.set(ctx, s, in.Model)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		return jsonResult(t.r.modelStatus(v.Model))
	}
	return protocol.ToolResult{}, fmt.Errorf("model: unknown action %q (valid actions: status, set, list — there is no clear action)", in.Action)
}

// set moves s to the model that ref names, or to the model of the alias ref.
func (t modelTool) set(ctx context.Context, s *Session, ref string) (protocol.Session, error) {
	if ref == "" {
		return protocol.Session{}, fmt.Errorf("model: set requires a non-empty model (%s)", t.r.modelChoices())
	}
	if target, ok := t.r.aliases[ref]; ok {
		ref = target
	}
	parsed, err := message.ParseModelRef(ref)
	if err != nil {
		return protocol.Session{}, fmt.Errorf("model: %w (%s)", err, t.r.modelChoices())
	}
	if err := t.r.servesProvider(parsed); err != nil {
		return protocol.Session{}, fmt.Errorf("model: %w", err)
	}
	v, err := s.Update(ctx, protocol.SettingsPatch{Model: &ref})
	if err != nil {
		return protocol.Session{}, fmt.Errorf("model: %w", err)
	}
	return v, nil
}

func jsonResult(v any) (protocol.ToolResult, error) {
	b, err := json.Marshal(v)
	return protocol.ToolResult{Text: string(b)}, err
}

func (r *Runtime) modelStatus(model string) modelStatus {
	return modelStatus{Model: model, Aliases: r.aliasCopy(), Providers: r.providerInfos()}
}

func (r *Runtime) aliasCopy() map[string]string {
	if len(r.aliases) == 0 {
		return nil
	}
	return maps.Clone(r.aliases)
}

func (r *Runtime) providerInfos() []providerInfo {
	names := r.models.Providers()
	if len(names) == 0 {
		return nil
	}
	infos := make([]providerInfo, len(names))
	for i, n := range names {
		infos[i] = providerInfo{Name: n, Billing: billing(n)}
	}
	return infos
}

// servesProvider reports a model whose provider no backend serves, and names
// the valid aliases and providers.
func (r *Runtime) servesProvider(ref message.ModelRef) error {
	for _, n := range r.models.Providers() {
		if n == ref.Provider {
			return nil
		}
	}
	return fmt.Errorf("provider %q is not configured (%s)", ref.Provider, r.modelChoices())
}

// modelChoices renders the valid aliases and provider names for an error
// that rejects a model.
func (r *Runtime) modelChoices() string {
	return fmt.Sprintf("valid aliases: %v; configured providers: %v", slices.Sorted(maps.Keys(r.aliases)), r.models.Providers())
}

// checkChildModel reports why no child can run model: it is not a ref, or no configured provider serves it.
func (r *Runtime) checkChildModel(model string) error {
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return fmt.Errorf("invalid model %q: %w", model, err)
	}
	return r.servesProvider(ref)
}
