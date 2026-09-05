// Package modelmeta provides static model context-window metadata.
//
// The tables are curated snapshots of models.dev's limit.context field for
// the model families that Harness serves. They remain static so session
// creation does not depend on a network request.
package modelmeta

import (
	"regexp"
	"strings"

	"github.com/majorcontext/harness/message"
)

// anthropicContextWindows is models.dev's "anthropic" provider entries'
// limit.context field, snapshotted 2026-08-20. Keyed by the model ID exactly
// as it appears after "anthropic/" in a message.ModelRef (matches
// config.DefaultModel's "anthropic/claude-fable-5" form).
var anthropicContextWindows = map[string]int{
	"claude-fable-5":             1_000_000,
	"claude-haiku-4-5":           200_000,
	"claude-haiku-4-5-20251001":  200_000,
	"claude-opus-4-5":            200_000,
	"claude-opus-4-5-20251101":   200_000,
	"claude-opus-4-6":            1_000_000,
	"claude-opus-4-7":            1_000_000,
	"claude-opus-4-8":            1_000_000,
	"claude-opus-5":              1_000_000,
	"claude-sonnet-4-5":          1_000_000,
	"claude-sonnet-4-5-20250929": 1_000_000,
	"claude-sonnet-4-6":          1_000_000,
	"claude-sonnet-5":            1_000_000,
}

// openaiContextWindows is models.dev's "openai" provider entries'
// limit.context field, snapshotted 2026-08-20. Image and embedding models
// (limit.context == 0 in the source catalog — they are not chat models) are
// deliberately omitted, since a zero entry here would be indistinguishable
// from "not found" and this table only needs to answer "what is the CHAT
// context window for this ref".
var openaiContextWindows = map[string]int{
	"gpt-3.5-turbo":       16_385,
	"gpt-4":               8_192,
	"gpt-4-turbo":         128_000,
	"gpt-4.1":             1_047_576,
	"gpt-4.1-mini":        1_047_576,
	"gpt-4.1-nano":        1_047_576,
	"gpt-4o":              128_000,
	"gpt-4o-2024-05-13":   128_000,
	"gpt-4o-2024-08-06":   128_000,
	"gpt-4o-2024-11-20":   128_000,
	"gpt-4o-mini":         128_000,
	"gpt-5":               400_000,
	"gpt-5-mini":          400_000,
	"gpt-5-nano":          400_000,
	"gpt-5-pro":           400_000,
	"gpt-5.1":             400_000,
	"gpt-5.2":             400_000,
	"gpt-5.2-chat-latest": 128_000,
	"gpt-5.2-pro":         400_000,
	"gpt-5.3-chat-latest": 128_000,
	"gpt-5.3-codex":       400_000,
	"gpt-5.3-codex-spark": 128_000,
	"gpt-5.4":             1_050_000,
	"gpt-5.4-mini":        400_000,
	"gpt-5.4-nano":        400_000,
	"gpt-5.4-pro":         1_050_000,
	"gpt-5.5":             1_050_000,
	"gpt-5.5-pro":         1_050_000,
	"gpt-5.6":             1_050_000,
	"gpt-5.6-luna":        1_050_000,
	"gpt-5.6-sol":         1_050_000,
	"gpt-5.6-terra":       1_050_000,
	"gpt-realtime-2.1":    128_000,
	"o1":                  200_000,
	"o1-pro":              200_000,
	"o3":                  200_000,
	"o3-mini":             200_000,
	"o3-pro":              200_000,
	"o4-mini":             200_000,
}

// bedrockAnthropicContextWindows is models.dev's "amazon-bedrock" provider
// entries' limit.context field for the anthropic.* model family,
// snapshotted 2026-08-20, keyed by the model ID with any region prefix
// ("us.", "eu.", "au.", "jp.", "global.") AND the leading "anthropic." family
// segment already stripped (see stripBedrockAnthropicPrefix) — every region
// variant of a given model reports the same limit.context in the source
// catalog, so one entry covers all of them.
//
// Every key here is bare: a trailing bedrock version suffix ("-vN" or
// "-vN:M", e.g. "-v1" or "-v1:0") is stripped before lookup (see
// stripBedrockVersionSuffix) rather than encoded in the key. models.dev's
// raw IDs are themselves inconsistent about carrying this suffix — some
// entries have it (e.g. the source ID behind "claude-sonnet-4-5-20250929"
// is "claude-sonnet-4-5-20250929-v1:0"), some don't (e.g. "claude-opus-4-8"
// has no suffixed form at all) — so a table keyed verbatim on the source ID
// silently misses whichever form (bare vs. suffixed) a caller happens to
// query with. Verified against https://models.dev/api.json 2026-08-20: the
// amazon-bedrock claude-sonnet-4-5-20250929-v1:0 entry genuinely reports
// limit.context == 200_000, distinct from (and NOT a snapshot error next
// to) the first-party anthropic/claude-sonnet-4-5 entry's 1_000_000 — the
// two routes report different windows for what is otherwise the same
// model family, and this table intentionally preserves that divergence.
var bedrockAnthropicContextWindows = map[string]int{
	"claude-fable-5":             1_000_000,
	"claude-haiku-4-5-20251001":  200_000,
	"claude-opus-4-1-20250805":   200_000,
	"claude-opus-4-5-20251101":   200_000,
	"claude-opus-4-6":            1_000_000,
	"claude-opus-4-7":            1_000_000,
	"claude-opus-4-8":            1_000_000,
	"claude-opus-5":              1_000_000,
	"claude-sonnet-4-5-20250929": 200_000,
	"claude-sonnet-4-6":          1_000_000,
	"claude-sonnet-5":            1_000_000,
}

// ContextWindow reports ref's advertised context window in tokens. It returns
// false for an unrecognized provider or model.
//
// ref.Model is normalized before lookup because the boxes platform
// (meetneptune/boxes internal/api/bifrost_models.go) passes THREE-segment
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
		// meetneptune/boxes internal/api/codex_models.go, which mints refs
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
	case "amazon-bedrock":
		if suffix, isAnthropic := stripBedrockAnthropicPrefix(model); isAnthropic {
			tokens, ok = bedrockAnthropicContextWindows[stripBedrockVersionSuffix(suffix)]
		}
	case claudeCodeProvider:
		// A turn delegated to the Claude Code CLI (see
		// engine/claude_code_backend.go) is driven entirely by that CLI's
		// OWN context management: it runs its own tool loop and its own
		// compaction over its own history, never harness's. This entry
		// exists ONLY so engine.Config.RequireContextWindow (default true
		// — an unrecognized model is a hard session-create refusal, see
		// engine/context_window.go) does not refuse a claude-code model
		// ref outright; harness's OWN automatic-compaction threshold is
		// unconditionally skipped for a delegated turn regardless of what
		// this reports (see PromptWithOrigin's early dispatch), so the
		// exact figure here drives no real behavior. claudeCodeContextWindow
		// (200,000, Sonnet's advertised first-party window) is a stand-in
		// chosen only to be an honest, plausible-sounding number rather
		// than an arbitrary placeholder like 0 or MaxInt.
		tokens, ok = claudeCodeContextWindow, true
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
// meetneptune/boxes internal/api/codex_models.go, e.g. "codex/gpt-5.6-sol")
// — distinct from provider/openai.CodexFamily, which names an "openai"-type
// provider's Client.Family for the same backend's wire format, not a
// message.ModelRef.Provider value this package switches on.
const codexProvider = "codex"

// claudeCodeContextWindow is the stand-in context-window figure reported
// for claudeCodeProvider — see the ContextWindow case above for why its
// exact value carries no real weight.
const claudeCodeContextWindow = 200_000

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
