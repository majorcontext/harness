// Package modelmeta is harness's built-in table of model context windows —
// the "per-model table" config.Config.ContextWindowTokens's doc comment used
// to say the engine lacked (see the jumpy-pizza incident:
// majorcontext/harness — a box died with "context exhausted: prompt 1136916
// tokens > limit 1000000" because ContextWindowTokens is opt-in and nothing
// on the boxes platform ever set it, so automatic compaction never armed).
//
// Two model catalogs were investigated as the metadata source:
//
//   - Bifrost's GET /v1/models (github.com/maximhq/bifrost, the gateway
//     harness's anthropic/openai-compat adapters talk to on the boxes
//     platform — see provider/anthropic, provider/openaicompat). Confirmed
//     live against Bifrost's own docs (docs.getbifrost.ai) and the PR that
//     added the endpoint (maximhq/bifrost#645): the response is the bare
//     OpenAI listing shape, {"data": [{"id", "object", "created",
//     "owned_by"}]} — NO context-length field at all. Bifrost aggregates
//     whatever its configured upstreams report, and none of the upstreams
//     this repo talks to natively (Anthropic, OpenAI) advertise context
//     length on their own /v1/models either. Ruled out as a metadata
//     source.
//   - models.dev's catalog (https://models.dev/api.json — also mirrored as
//     the `models.dev` npm package). Each entry carries a `limit` object
//     with `context` (input context window, in tokens) and `output` (max
//     output tokens); see e.g. the "anthropic" and "amazon-bedrock" top-level
//     keys, each a map of model ID -> entry. Verified live on 2026-08-20:
//     anthropic/claude-fable-5 (this repo's config.DefaultModel) reports
//     limit.context == 1000000 — the EXACT limit jumpy-pizza's incident
//     error named, confirming this is the right source and field.
//
// This table is a curated snapshot of that `limit.context` field for the
// model families harness's native providers (provider/anthropic,
// provider/openai) and the amazon-bedrock family the boxes platform routes
// through actually serve — not the full models.dev catalog. It is
// deliberately static (no network call): NewSession's doc comment already
// promises "nothing touches the network... on this path", and a box's
// automatic-compaction arming must not depend on models.dev being reachable
// at session-create time. Refresh by re-running the snapshot against
// models.dev/api.json; each map below cites the date it was last verified.
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

// ContextWindow reports ref's advertised context window in tokens, sourced
// from the tables above. ok is false when ref names a model this table has
// no entry for (an unrecognized provider, or a model newer than the last
// snapshot) — the caller's job to decide what "unknown" means (see
// engine.resolveContextWindow: unknown behaves exactly like "no metadata",
// i.e. automatic compaction stays disabled, matching today's behavior for
// every model this table doesn't yet know about).
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
// automatic compaction never arms on the platform this package exists to
// serve (see the jumpy-pizza incident cited in this file's package doc).
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
