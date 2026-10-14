// Package modelmeta provides static model context-window and display metadata.
//
// The tables are curated snapshots of models.dev's limit.context field for
// the model families that Harness serves. They remain static so session
// creation does not depend on a network request. "go generate" refreshes
// them from models.dev; overrides.json holds the entries models.dev lacks.
package modelmeta

//go:generate go run ./internal/genctx

import (
	"cmp"
	"regexp"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/modelref"
)

// ContextWindow reports ref's advertised context window in tokens. It returns
// false for an unrecognized provider or model. A claude-code ref is
// unrecognized: the CLI resolves a bare alias itself, so its backend owns the
// window and reports it during a turn.
//
// ref.Model is normalized before lookup because the boxes platform
// (majorcontext/bailey internal/api/bifrost_models.go) passes THREE-segment
// refs exclusively — e.g. "anthropic/anthropic/claude-fable-5" or
// "anthropic/bedrock_mantle/anthropic.claude-opus-5" — and
// modelref.ParseModelRef splits on the FIRST slash only (see ModelRef's doc
// comment: "the model portion may itself contain slashes"), so ref.Model
// still carries a Bifrost routing-namespace segment ("anthropic",
// "bedrock_mantle", "bedrock", ...) ahead of the actual model ID. Without
// stripping that segment first, EVERY box ref misses this table and
// automatic compaction does not arm for those refs.
func ContextWindow(ref modelref.ModelRef) (tokens int, ok bool) {
	model := lastPathSegment(ref.Model)
	switch ref.Provider {
	case "anthropic":
		// A bedrock/mantle-routed ref's namespace-stripped model still
		// carries the dotted "anthropic." family segment (Bifrost's raw
		// bedrock-style ID, e.g. "anthropic.claude-opus-5-v1:0") ahead of
		// the model ID proper; the direct-vendor form (e.g.
		// "claude-fable-5") never does. The dotted prefix marks a ref
		// that is SERVED through Bedrock, so it must honor the Bedrock
		// window where the two tables diverge (see
		// bedrockAnthropicContextWindows's doc comment).
		// Over-reporting here arms compaction above the route's real
		// limit and re-creates the overflow this package exists to
		// prevent.
		//
		// stripBedrockAnthropicPrefix (not a bare CutPrefix) so a region
		// segment ("us."/"eu."/"global.") is tolerated symmetrically with
		// the amazon-bedrock branch below. Bedrock-served refs consult the
		// bedrock table EXCLUSIVELY — no first-party fallback: a dotted
		// family the bedrock snapshot doesn't key resolves as unknown
		// (the backend applies its default window) rather than
		// borrowing the first-party window, which would silently un-do the
		// divergence for any form not keyed exactly.
		if suffix, isBedrockStyle := stripBedrockAnthropicPrefix(model); isBedrockStyle {
			tokens, ok = bedrockAnthropicContextWindows[stripBedrockVersionSuffix(suffix)]
			return tokens, ok
		}
		tokens, ok = anthropicContextWindows[stripBedrockVersionSuffix(model)]
	case "openai":
		if suffix, ok2 := strings.CutPrefix(model, "openai."); ok2 {
			model = stripBedrockVersionSuffix(suffix)
		}
		tokens, ok = openaiContextWindows[model]
	case codexProvider:
		// A ref routed through the ChatGPT Codex backend (see
		// majorcontext/bailey internal/api/codex_models.go, which mints refs
		// like "codex/gpt-5.6-sol") names the SAME underlying OpenAI model
		// its "openai/*" counterpart does — openaiContextWindows already
		// keys every codex model boxes uses (gpt-5.6-sol, gpt-5.6-terra,
		// gpt-5.6-luna) — so this case looks the model up in that one
		// table rather than duplicating it. A codex model absent from the
		// table misses and the model runs on the default window.
		tokens, ok = openaiContextWindows[model]
	case "bifrost":
		if tokens, ok = bifrostFireworksContextWindows[model]; ok {
			return tokens, ok
		}
		tokens, ok = bifrostVertexContextWindows[model]
	case "amazon-bedrock":
		if suffix, isAnthropic := stripBedrockAnthropicPrefix(model); isAnthropic {
			tokens, ok = bedrockAnthropicContextWindows[stripBedrockVersionSuffix(suffix)]
		}
	}
	return tokens, ok
}

// codexModels are the models that the ChatGPT subscription backend accepts.
// It refuses most other OpenAI models, gpt-5.3-codex included.
var codexModels = []string{"gpt-6.1-sol", "gpt-6-sol", "gpt-6-astra", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5"}

// claudeCodeModels are the model aliases that the Claude Code CLI resolves,
// with the name that a client shows for each.
var claudeCodeModels = []Model{
	{ID: "fable", Name: "Claude Fable (Claude Code)", Efforts: claudeCodeEfforts},
	{ID: "haiku", Name: "Claude Haiku (Claude Code)", Efforts: claudeCodeEfforts},
	{ID: "opus", Name: "Claude Opus (Claude Code)", Efforts: claudeCodeEfforts},
	{ID: "sonnet", Name: "Claude Sonnet (Claude Code)", Efforts: claudeCodeEfforts},
}

// unlistedEfforts are the levels of a model that no table holds.
var unlistedEfforts = []string{"off", "minimal", "low", "medium", "high"}

// claudeCodeEfforts are the levels of a model whose backend decides what it takes.
var claudeCodeEfforts = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// modelInfo holds the models.dev facts of one chat model. A model that is not
// listed is deprecated or cannot call tools.
type modelInfo struct {
	name, description, released string
	efforts                     []string
	// effortField is true when the model takes a level as a wire effort field,
	// and budgetTokens is true when it takes a thinking budget.
	effortField, budgetTokens bool
	listed                    bool
	// noImageInput is set when models.dev lists the input modalities of the model and image is not one.
	noImageInput bool
}

// Model is a model that a provider serves, with the facts a client shows.
type Model struct {
	// ID is the model id without the provider.
	ID string
	// Name is a human label, or empty when unknown.
	Name string
	// Description is one line about the model, or empty when unknown.
	Description string
	// Efforts are the effort levels that the model supports, from off to
	// max. It is empty for a model that has no reasoning control.
	Efforts []string
}

// datedSnapshotPattern matches a trailing snapshot date of eight digits, with
// or without hyphens.
var datedSnapshotPattern = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

// Models returns the models that a provider serves, newest first and then by
// name. The anthropic and openai models are the listed chat entries of the
// generated tables, with a dated snapshot folded into its undated alias when
// the table holds both; a deprecated model and a model without tool calls are
// not listed, although ContextWindow still knows them. The codex models are
// ones that the subscription backend accepts. The claude-code aliases are
// names that the CLI resolves, and the backend owns their window. It returns
// nil for a provider with no curated list.
func Models(provider string) []Model {
	switch provider {
	case "anthropic":
		return listed(undatedKeys(anthropicContextWindows), anthropicInfo)
	case "openai":
		return listed(undatedKeys(openaiContextWindows), openaiInfo)
	case codexProvider:
		return CodexModels()
	case claudeCodeProvider:
		out := slices.Clone(claudeCodeModels)
		for i := range out {
			out[i].Efforts = slices.Clone(out[i].Efforts)
		}
		return out
	}
	return nil
}

// Named returns the models of ids, in the order given, for a provider whose entry lists its models. A model
// of catalog, the curated list of the provider, has the facts of the catalog. Any other model that the tables
// hold has its models.dev name, description, and efforts; the id is read as ContextWindow reads it, so a
// routing namespace such as "anthropic/" does not hide the model. Any other id has no name and no description,
// and every effort level up to high, as a model that no list holds has.
func Named(ids []string, catalog []Model) []Model {
	out := make([]Model, len(ids))
	for n, id := range ids {
		out[n] = Model{ID: id, Efforts: slices.Clone(unlistedEfforts)}
		if i := slices.IndexFunc(catalog, func(m Model) bool { return m.ID == id }); i >= 0 {
			out[n] = catalog[i]
			out[n].Efforts = slices.Clone(out[n].Efforts)
		} else if i, ok := factsOf(id); ok {
			out[n] = Model{ID: id, Name: i.name, Description: i.description, Efforts: slices.Clone(i.efforts)}
		}
	}
	return out
}

// factsOf finds the models.dev facts of a model id of any family, with the normalization of ContextWindow.
func factsOf(id string) (modelInfo, bool) {
	model := lastPathSegment(id)
	if suffix, bedrock := stripBedrockAnthropicPrefix(model); bedrock {
		model = suffix
	}
	if i, ok := anthropicInfo[stripBedrockVersionSuffix(model)]; ok {
		return i, true
	}
	i, ok := openaiInfo[stripBedrockVersionSuffix(strings.TrimPrefix(model, "openai."))]
	return i, ok
}

// CodexModels returns the static list of the models that the ChatGPT subscription backend accepts, in the order of Models.
func CodexModels() []Model { return listed(codexModels, openaiInfo) }

// OpenAIModel returns the models.dev facts of an OpenAI model id, listed or not.
func OpenAIModel(id string) (Model, bool) {
	i, ok := openaiInfo[id]
	if !ok {
		return Model{}, false
	}
	return Model{ID: id, Name: i.name, Description: i.description, Efforts: slices.Clone(i.efforts)}, true
}

// ImageInput reports whether the model of ref accepts an image as input, from
// the models.dev input modalities. known is false for a model that the tables
// do not hold or whose input modalities models.dev does not list; the caller
// must then assume that the backend decides.
func ImageInput(ref modelref.ModelRef) (accepts, known bool) {
	i, ok := infoOf(ref)
	if !ok {
		return false, false
	}
	return !i.noImageInput, true
}

// infoOf finds the models.dev facts of ref with the normalization that ContextWindow applies.
func infoOf(ref modelref.ModelRef) (modelInfo, bool) {
	model := lastPathSegment(ref.Model)
	var i modelInfo
	var ok bool
	switch ref.Provider {
	case "anthropic", "amazon-bedrock":
		suffix, bedrock := stripBedrockAnthropicPrefix(model)
		switch {
		case bedrock:
			i, ok = anthropicInfo[stripBedrockVersionSuffix(suffix)]
		case ref.Provider == "anthropic":
			i, ok = anthropicInfo[stripBedrockVersionSuffix(model)]
		}
	case "openai":
		if suffix, cut := strings.CutPrefix(model, "openai."); cut {
			model = stripBedrockVersionSuffix(suffix)
		}
		i, ok = openaiInfo[model]
	case codexProvider:
		i, ok = openaiInfo[model]
	}
	return i, ok
}

// ProviderName returns the display name of a provider from models.dev, or
// an empty string when it has none.
func ProviderName(provider string) string { return providerNames[provider] }

// listed returns the models of ids that info lists, ordered by release date
// with the newest first and then by name.
func listed(ids []string, info map[string]modelInfo) []Model {
	type row struct {
		Model
		released string
	}
	var rows []row
	for _, id := range ids {
		i, ok := info[id]
		if !ok || !i.listed {
			continue
		}
		rows = append(rows, row{Model{ID: id, Name: i.name, Description: i.description, Efforts: slices.Clone(i.efforts)}, i.released})
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if c := strings.Compare(b.released, a.released); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	out := make([]Model, len(rows))
	for i, r := range rows {
		out[i] = r.Model
	}
	return out
}

// undatedKeys returns the keys of table without a dated snapshot whose
// undated alias is also a key.
func undatedKeys(table map[string]int) []string {
	out := make([]string, 0, len(table))
	for id := range table {
		if base := datedSnapshotPattern.ReplaceAllString(id, ""); base != id {
			if _, ok := table[base]; ok {
				continue
			}
		}
		out = append(out, id)
	}
	return out
}

// claudeCodeProvider is the modelref.ModelRef.Provider value that selects
// the Claude Code CLI delegated-turn backend (config.TypeClaudeCodeCLI).
const claudeCodeProvider = "claude-code"

// codexProvider is the modelref.ModelRef.Provider value the boxes platform
// mints for a ChatGPT Codex backend model (see
// majorcontext/bailey internal/api/codex_models.go, e.g. "codex/gpt-5.6-sol")
// — distinct from internal/provider/openai.CodexFamily, which names an "openai"-type
// provider's Client.Family for the same backend's wire format, not a
// modelref.ModelRef.Provider value this package switches on.
const codexProvider = "codex"

// lastPathSegment returns the substring of model after its last '/', or
// model unchanged if it contains no '/'. modelref.ModelRef.Model may itself
// contain slashes (see that type's doc comment), which is exactly what the
// boxes platform's three-segment refs put there — a Bifrost routing-
// namespace segment ("anthropic", "bedrock_mantle", "bedrock", ...) ahead
// of the real model ID. This table is keyed on the bare model ID, so that
// namespace prefix — whatever it is — must come off before any lookup.
func lastPathSegment(model string) string {
	if idx := strings.LastIndexByte(model, '/'); idx >= 0 {
		return model[idx+1:]
	}
	return model
}

// bedrockVersionSuffixPattern matches a trailing bedrock-style version
// suffix: "-v" followed by digits, optionally ":" followed by more digits
// (e.g. "-v1" or "-v1:0"). Anchored to the end of the string so it only
// ever strips a genuine trailing version marker, never a hyphenated digit
// that happens to appear elsewhere in a model ID (e.g. "-4-5" in
// "claude-opus-4-5" has no "v", so it never matches).
var bedrockVersionSuffixPattern = regexp.MustCompile(`-v\d+(:\d+)?$`)

// stripBedrockVersionSuffix removes a trailing bedrock-style version
// suffix from model, if present — see bedrockVersionSuffixPattern and
// bedrockAnthropicContextWindows's doc comment for why the suffix must be
// normalized away rather than matched literally: models.dev's raw bedrock
// IDs are themselves inconsistent about carrying it.
func stripBedrockVersionSuffix(model string) string {
	return bedrockVersionSuffixPattern.ReplaceAllString(model, "")
}

// stripBedrockAnthropicPrefix strips an amazon-bedrock model ID's region
// prefix (a single lowercase segment before "anthropic.", e.g. "us.", "eu.",
// "global." — AWS adds new regions over time, so this is pattern-matched
// rather than an enumerated list) and the "anthropic." family segment
// itself, returning the remaining suffix and whether the ID was an
// anthropic.* model at all (a non-anthropic bedrock model, e.g. a Titan or
// Llama ID, reports isAnthropic == false — this table has no entry for it).
func stripBedrockAnthropicPrefix(model string) (suffix string, isAnthropic bool) {
	rest, ok := strings.CutPrefix(model, "anthropic.")
	if ok {
		return rest, true
	}
	// Cut splits at the FIRST ".", so tail here is everything after exactly
	// one leading segment — a two-segment "region" (e.g. "a.b.") can never
	// reach this point with tail beginning "anthropic.", so it falls through
	// to the final false below without a separate check.
	_, tail, found := strings.Cut(model, ".")
	if !found {
		return "", false
	}
	rest, ok = strings.CutPrefix(tail, "anthropic.")
	if !ok {
		return "", false
	}
	return rest, true
}

// offThinkingDisabled is the thinking type that off sends unless the model names another.
const offThinkingDisabled = "disabled"

// EffortControl is how a model takes a reasoning level.
type EffortControl struct {
	// Levels are the levels that the model lists.
	Levels []string
	// Field is true when the model takes a level as a wire effort field.
	Field bool
	// Budget is true when the model takes a thinking budget.
	Budget bool
	// ThinksByDefault is true when a request that omits the thinking field
	// runs adaptive thinking.
	ThinksByDefault bool
	// OffThinking is the thinking type that off sends, "disabled" or
	// "between_tools". It is empty when off sends no thinking field.
	OffThinking string
}

// AnthropicEfforts returns the effort control of an Anthropic model id, with
// a Bedrock-style id and a routing namespace read as the model they name. It
// returns false for a model that the table does not hold.
func AnthropicEfforts(model string) (EffortControl, bool) {
	model = lastPathSegment(model)
	if suffix, bedrock := stripBedrockAnthropicPrefix(model); bedrock {
		model = suffix
	}
	model = stripBedrockVersionSuffix(model)
	c, ok := controlOf(anthropicInfo, model)
	c.ThinksByDefault = ok && c.Field && !c.Budget && !thinkingOffByDefault[model]
	if c.ThinksByDefault && slices.Contains(c.Levels, "off") {
		c.OffThinking = cmp.Or(anthropicThinkingOffType[model], offThinkingDisabled)
	}
	return c, ok
}

// OpenAIEfforts returns the effort control of an OpenAI model id, or false for
// a model that the table does not hold.
func OpenAIEfforts(model string) (EffortControl, bool) {
	model = strings.TrimPrefix(lastPathSegment(model), "openai.")
	return controlOf(openaiInfo, model)
}

// ClaudeCodeEfforts returns the levels that the Claude Code CLI takes.
func ClaudeCodeEfforts() []string { return slices.Clone(claudeCodeEfforts) }

func controlOf(info map[string]modelInfo, id string) (EffortControl, bool) {
	i, ok := info[id]
	if !ok {
		return EffortControl{}, false
	}
	return EffortControl{Levels: slices.Clone(i.efforts), Field: i.effortField, Budget: i.budgetTokens}, true
}
