// Package modelmeta provides static model context-window metadata.
//
// The tables are curated snapshots of models.dev's limit.context field for
// the model families that Harness serves. They remain static so session
// creation does not depend on a network request. "go generate" refreshes
// them from models.dev; overrides.json holds the entries models.dev lacks.
package modelmeta

//go:generate go run ./internal/genctx

import (
	"regexp"
	"strings"

	"github.com/majorcontext/harness/message"
)

// ContextWindow reports ref's advertised context window in tokens. It returns
// false for an unrecognized provider or model, and 0 with true for a
// claude-code ref, which is recognized but has no knowable window: the CLI
// resolves a bare alias itself and never reports its choice. A caller must
// treat that 0 as "unknown", never as a usable size.
//
// ref.Model is normalized before lookup because the boxes platform
// (majorcontext/bailey internal/api/bifrost_models.go) passes THREE-segment
// refs exclusively — e.g. "anthropic/anthropic/claude-fable-5" or
// "anthropic/bedrock_mantle/anthropic.claude-opus-5" — and
// message.ParseModelRef splits on the FIRST slash only (see ModelRef's doc
// comment: "the model portion may itself contain slashes"), so ref.Model
// still carries a Bifrost routing-namespace segment ("anthropic",
// "bedrock_mantle", "bedrock", ...) ahead of the actual model ID. Without
// stripping that segment first, EVERY box ref misses this table and
// automatic compaction does not arm for those refs.
func ContextWindow(ref message.ModelRef) (tokens int, ok bool) {
	model := lastPathSegment(ref.Model)
	switch ref.Provider {
	case "anthropic":
		// A bedrock/mantle-routed ref's namespace-stripped model still
		// carries the dotted "anthropic." family segment (Bifrost's raw
		// bedrock-style ID, e.g. "anthropic.claude-opus-5-v1:0") ahead of
		// the model ID proper; the direct-vendor form (e.g.
		// "claude-fable-5") never does. The dotted prefix marks a ref
		// that is SERVED through Bedrock, so it must honor the Bedrock
		// window where the two tables diverge (today only
		// claude-sonnet-4-5-20250929: 200k on Bedrock vs 1M first-party
		// — see bedrockAnthropicContextWindows's doc comment).
		// Over-reporting here arms compaction above the route's real
		// limit and re-creates the overflow this package exists to
		// prevent.
		//
		// stripBedrockAnthropicPrefix (not a bare CutPrefix) so a region
		// segment ("us."/"eu."/"global.") is tolerated symmetrically with
		// the amazon-bedrock branch below. Bedrock-served refs consult the
		// bedrock table EXCLUSIVELY — no first-party fallback: a dotted
		// family the bedrock snapshot doesn't key resolves as unknown
		// (compaction stays disabled, the fail-safe direction) rather than
		// borrowing the first-party window, which would silently un-do the
		// divergence for any form not keyed exactly (e.g. the undated
		// "anthropic.claude-sonnet-4-5" borrowing 1M where Bedrock's real
		// window is 200k).
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
		// table rather than duplicating it. Unlike claudeCodeProvider
		// below, there is no stand-in fallback: a codex model absent from
		// the table still misses, so engine.Config.RequireContextWindow's
		// fail-loud refusal (see engine/context_window.go) stays armed for
		// a genuinely unknown model instead of a boxes-side override
		// disabling it globally.
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
	case claudeCodeProvider:
		// The CLI resolves a bare alias itself, so no window here can be
		// right. Report none rather than a plausible figure: a wrong
		// denominator renders a session five times fuller than it is. A
		// turn's "result" envelope carries the window the CLI chose, and
		// applyClaudeCodeUsage reports that instead.
		tokens, ok = 0, true
	}
	return tokens, ok
}

// claudeCodeProvider is the message.ModelRef.Provider value that selects
// the Claude Code CLI delegated-turn backend (engine/claude_code_backend.go
// and config.TypeClaudeCodeCLI) — duplicated here, rather than imported,
// because package modelmeta must not depend on package engine (engine
// already depends on modelmeta for this very function). Kept in sync by
// engine's TestClaudeCodeProviderFamilyMatchesModelmeta.
const claudeCodeProvider = "claude-code"

// codexProvider is the message.ModelRef.Provider value the boxes platform
// mints for a ChatGPT Codex backend model (see
// majorcontext/bailey internal/api/codex_models.go, e.g. "codex/gpt-5.6-sol")
// — distinct from provider/openai.CodexFamily, which names an "openai"-type
// provider's Client.Family for the same backend's wire format, not a
// message.ModelRef.Provider value this package switches on.
const codexProvider = "codex"

// lastPathSegment returns the substring of model after its last '/', or
// model unchanged if it contains no '/'. message.ModelRef.Model may itself
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

// anthropicToolSearchModels is the set of first-party Anthropic model IDs
// that support the SERVER-side tool search tool
// (tool_search_tool_regex_20251119 / tool_search_tool_bm25_20251119), from
// the model-compatibility table in
// platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool.
// Both variants ship on exactly the same models, so one set answers for
// either.
//
// A set, not a table of versions: harness picks the variant (see
// provider/anthropic), so the only question this package answers is whether
// the ref can do server-side tool search at all. Claude Opus 4.1 and
// earlier cannot.
//
// Undated aliases are keyed alongside the dated IDs for the same reason
// anthropicContextWindows keys both: a config may name either form.
var anthropicToolSearchModels = map[string]bool{
	"claude-fable-5": true,
	// claude-mythos-5 is in the tool-search compatibility table but has no
	// models.dev entry (checked live: the anthropic provider's model map
	// has no mythos key at all) and no route this repo talks to serves it,
	// so anthropicContextWindows cannot key it either. Keeping it here is
	// the accurate answer if such a ref ever appears, and costs nothing
	// meanwhile; see TestToolSearchModelsAreKnownToContextWindow for the
	// self-retiring exemption that keeps the two tables honest.
	"claude-mythos-5":            true,
	"claude-haiku-4-5":           true,
	"claude-haiku-4-5-20251001":  true,
	"claude-opus-4-5":            true,
	"claude-opus-4-5-20251101":   true,
	"claude-opus-4-6":            true,
	"claude-opus-4-7":            true,
	"claude-opus-4-8":            true,
	"claude-opus-5":              true,
	"claude-sonnet-4-5":          true,
	"claude-sonnet-4-5-20250929": true,
	"claude-sonnet-4-6":          true,
	"claude-sonnet-5":            true,
}

// SupportsToolSearch reports whether ref can use Anthropic's server-side
// tool search tool. It is the gate provider/anthropic gives native
// delegation: a ref this answers false for keeps harness's own client-side
// deferral (the catalog segment plus the mcp tool's search/select actions),
// which works on every provider.
//
// Two deliberate fail-safe-off rules, both of which make an unknown ref
// keep the client-side mechanism rather than emit a tool the route may
// reject:
//
//   - Only ref.Provider "anthropic" can be true. The openai and
//     openai-compat routes reach a Chat Completions surface, which has no
//     tool_search at all (OpenAI's own tool search is Responses-API only),
//     and a gateway that proxies Anthropic under some other provider name
//     is not something this package can recognize.
//   - A BEDROCK-STYLE anthropic ref is false, whatever model it names.
//     Server-side tool search on Amazon Bedrock is available only through
//     InvokeModel, not the Converse API, and nothing in a ref says which
//     API the gateway in front of it uses. Guessing wrong costs a rejected
//     request on every turn; guessing off costs a catalog segment that
//     already works. Same conservative posture bedrockAnthropicContextWindows
//     takes for a family its snapshot does not key.
//
// The Bifrost routing-namespace segment is stripped exactly as
// ContextWindow strips it (lastPathSegment), so a boxes ref like
// "anthropic/claude-opus-5" resolves.
func SupportsToolSearch(ref message.ModelRef) bool {
	if ref.Provider != "anthropic" {
		return false
	}
	model := lastPathSegment(ref.Model)
	if _, isBedrockStyle := stripBedrockAnthropicPrefix(model); isBedrockStyle {
		return false
	}
	return anthropicToolSearchModels[model]
}
