package selftool

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// ModelName is the name of the model tool.
const ModelName = "model"

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

const modelDeferredDescription = "Inspect or change this session's model. status reports the current model and any pending_model, plus configured aliases and providers. " +
	"set(model) validates a provider/model ref or alias now and queues it for the NEXT TURN, after this turn ends, even on failure or cancellation. " +
	"It never changes the running turn or its backend process. A later set replaces the pending model; setting the current model clears it and does nothing. " +
	"list reports configured providers (with subscription or api billing) and aliases without session state."

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
	PendingModel string            `json:"pending_model,omitempty"`
	Effect       string            `json:"effect,omitempty"`
	Model        string            `json:"model"`
	Aliases      map[string]string `json:"aliases,omitempty"`
	Providers    []providerInfo    `json:"providers,omitempty"`
}

type modelList struct {
	Providers []providerInfo    `json:"providers"`
	Aliases   map[string]string `json:"aliases,omitempty"`
}

// Catalog is what the runtime configures for the models of its sessions.
type Catalog struct {
	// Aliases maps an alias to a "provider/model" ref.
	Aliases map[string]string
	// Providers returns the names of the configured providers.
	Providers func() []string
}

// Model is the model tool for the session to which the runtime binds it.
type Model struct {
	host     Sessions
	catalog  Catalog
	session  string
	deferred bool
}

// NewModel returns the model tool, unbound, over host and catalog.
func NewModel(host Sessions, catalog Catalog) Model { return Model{host: host, catalog: catalog} }

// Deferred returns the tool whose model change waits for the next turn.
func (t Model) Deferred() Model {
	t.deferred = true
	return t
}

// Spec returns the specification of the model tool.
func (t Model) Spec() protocol.ToolSpec {
	if t.deferred {
		return protocol.ToolSpec{Name: ModelName, Description: modelDeferredDescription, InputSchema: json.RawMessage(modelSchema)}
	}
	return protocol.ToolSpec{Name: ModelName, Description: modelDescription, InputSchema: json.RawMessage(modelSchema)}
}

// Bind returns the tool of session id.
func (t Model) Bind(id string, _ bool, _ int) turn.Tool {
	t.session = id
	return t
}

// Alone makes the model tool run with no other call of its model call in flight.
func (Model) Alone() {}

// Run performs the action of c.
func (t Model) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Action, Model string }
	if err := json.Unmarshal(c.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("model: invalid arguments: %w", err)
	}
	s, ok := t.host.Session(t.session)
	if !ok {
		return protocol.ToolResult{}, session.ErrNotOwned
	}
	switch {
	case in.Action == "list":
		return jsonResult(modelList{Providers: t.catalog.providerInfos(), Aliases: t.catalog.aliasCopy()})
	case in.Action == "status":
		return t.status(ctx, s, "")
	case in.Action == "set":
		v, err := t.set(ctx, s, in.Model)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		if t.deferred {
			return t.status(ctx, s, "next_turn")
		}
		return jsonResult(t.catalog.modelStatus(v.Model))
	}
	return protocol.ToolResult{}, fmt.Errorf("model: unknown action %q (valid actions: status, set, list — there is no clear action)", in.Action)
}

func (t Model) status(ctx context.Context, s Session, effect string) (protocol.ToolResult, error) {
	v := t.catalog.modelStatus(s.Snapshot().Model)
	if t.deferred {
		var err error
		v.PendingModel, err = s.PendingModel(ctx)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		if effect != "" {
			v.Effect = effect
			if v.PendingModel == "" {
				v.Effect = "unchanged"
			}
		}
	}
	return jsonResult(v)
}

// set moves s to the model that ref names, or to the model of the alias ref.
func (t Model) set(ctx context.Context, s Session, ref string) (protocol.Session, error) {
	if ref == "" {
		return protocol.Session{}, fmt.Errorf("model: set requires a non-empty model (%s)", t.catalog.modelChoices())
	}
	if target, ok := t.catalog.Aliases[ref]; ok {
		ref = target
	}
	parsed, err := modelref.ParseModelRef(ref)
	if err != nil {
		return protocol.Session{}, fmt.Errorf("model: %w (%s)", err, t.catalog.modelChoices())
	}
	if err := t.catalog.servesProvider(parsed); err != nil {
		return protocol.Session{}, fmt.Errorf("model: %w", err)
	}
	if t.deferred {
		if err := s.DeferModel(ctx, ref); err != nil {
			return protocol.Session{}, fmt.Errorf("model: %w", err)
		}
		return s.Snapshot(), nil
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

func (c Catalog) modelStatus(model string) modelStatus {
	return modelStatus{Model: model, Aliases: c.aliasCopy(), Providers: c.providerInfos()}
}

func (c Catalog) aliasCopy() map[string]string {
	if len(c.Aliases) == 0 {
		return nil
	}
	return maps.Clone(c.Aliases)
}

func (c Catalog) providerInfos() []providerInfo {
	names := c.Providers()
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
func (c Catalog) servesProvider(ref modelref.ModelRef) error {
	for _, n := range c.Providers() {
		if n == ref.Provider {
			return nil
		}
	}
	return fmt.Errorf("provider %q is not configured (%s)", ref.Provider, c.modelChoices())
}

// modelChoices renders the valid aliases and provider names for an error
// that rejects a model.
func (c Catalog) modelChoices() string {
	return fmt.Sprintf("valid aliases: %v; configured providers: %v", slices.Sorted(maps.Keys(c.Aliases)), c.Providers())
}

// CheckChild reports why no child can run model: it is not a ref, or no configured provider serves it.
func (c Catalog) CheckChild(model string) error {
	ref, err := modelref.ParseModelRef(model)
	if err != nil {
		return err
	}
	return c.servesProvider(ref)
}
