package message

import "strings"

// EngineContext holds trusted runtime status. Only the engine creates it.
//
// Transcoders render its sentinels and neutralize those sentinels in Text parts. Engine context is request-only, though canonical JSON retains the part.

type EngineContext struct {
	// Text is the rendered block body, e.g. "[engine: harness 0.1.0-dev ...]".
	// The producer already renders the "[name: ...]" shape; this part only
	// tags that string as engine-originated.
	Text string `json:"text"`
}

func (*EngineContext) partType() PartType { return PartEngineContext }

// PartEngineContext is the JSON discriminator for an EngineContext part.
const PartEngineContext PartType = "engine_context"

// EngineContextOpenTag and EngineContextCloseTag wrap an engine-context block
// on the wire. Only RenderEngineContext (called by every transcoder for an
// *EngineContext part) emits them, and NeutralizeEngineContextSentinel strips
// them from any other text, so their presence on the wire is a signal only the
// engine can produce. The base system prompt (cmd/harness) names these exact
// tags when it tells the model what to trust, so the two never drift.
const (
	EngineContextOpenTag  = "<harness-engine-context>"
	EngineContextCloseTag = "</harness-engine-context>"
)

// neutralizedOpenTag and neutralizedCloseTag are the defanged forms
// NeutralizeEngineContextSentinel rewrites a literal sentinel into: visibly
// the same words, but with the angle brackets replaced by parentheses so the
// result is provably NOT the trusted tag. Deterministic and ASCII, so
// neutralizing is idempotent and never changes the cached request prefix
// across requests.
const (
	neutralizedOpenTag  = "(harness-engine-context)"
	neutralizedCloseTag = "(/harness-engine-context)"
)

// RenderEngineContext returns the wire text for an engine-context block: the
// sentinel-wrapped body every transcoder emits for an *EngineContext part.
// The body is itself run through NeutralizeEngineContextSentinel, so a
// runtime-declared process name or an MCP failure reason that happens to
// contain the close tag can never break out of the envelope and forge a
// second, nested block.
func RenderEngineContext(body string) string {
	return EngineContextOpenTag + "\n" + NeutralizeEngineContextSentinel(body) + "\n" + EngineContextCloseTag
}

// NeutralizeEngineContextSentinel defangs any literal engine-context sentinel
// tag in s, so only a genuine *EngineContext part (via RenderEngineContext)
// can emit the sentinel on the wire. Every transcoder runs each *Text part's
// content through this before emitting it. A collision with legitimate text is
// astronomically unlikely — the tag is a deliberately obscure fixed string —
// and the rewrite is a visible, harmless bracket swap, never a silent drop.
func NeutralizeEngineContextSentinel(s string) string {
	if !strings.Contains(s, EngineContextOpenTag) && !strings.Contains(s, EngineContextCloseTag) {
		return s
	}
	s = strings.ReplaceAll(s, EngineContextOpenTag, neutralizedOpenTag)
	s = strings.ReplaceAll(s, EngineContextCloseTag, neutralizedCloseTag)
	return s
}
