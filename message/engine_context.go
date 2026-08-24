package message

import "strings"

// EngineContext is an ambient status block the harness engine appends to the
// newest user message every request: engine identity ([engine: ...]),
// managed-process status ([processes: ...]), degraded-MCP status ([mcp: ...]),
// and the parked-goal notice ([goal: ...]). See engine/process.go's
// withAmbientStatus for the single producer.
//
// # Why a distinct part-kind, not a Text part
//
// These blocks were once appended as bare *Text parts (see NEP: the ambient
// trust-spoofing finding). A bare *Text block is BYTE-INDISTINGUISHABLE from
// user-typed or pasted text, so a payload a user pastes that happens to
// contain "[engine: ...]" inherited the same trust the engine's own block
// carries. A model told to trust bracketed status lines could then be spoofed
// by attacker-controlled text.
//
// EngineContext closes that at the canonical layer: it is a SEPARATE Go type
// with its own PartEngineContext discriminator, and only the engine's own
// withAmbientStatus produces one. A user- or paste-authored part is always a
// *Text, however its bytes are shaped — it can never BE an *EngineContext.
// Every canonical-layer consumer (a chat.message plugin hook, the server
// journal, this package's own tests) can therefore tell an engine block from
// user content by TYPE, not by re-parsing text.
//
// # The wire layer
//
// A provider only ever sees wire bytes, so the canonical distinction alone
// does not protect the model. Every transcoder renders an *EngineContext
// through RenderEngineContext, which wraps the block in the
// EngineContextOpenTag/EngineContextCloseTag sentinel, and renders every
// *Text through NeutralizeEngineContextSentinel, which defangs any literal
// sentinel a *Text carries. Only a genuine *EngineContext can therefore emit
// the sentinel on the wire, so the base system prompt can safely tell the
// model to trust the sentinel-wrapped block and to distrust bracketed text
// outside it. The rendering stays an ordinary text block on every provider —
// no new wire feature — so provider compatibility is unchanged.
//
// EngineContext is runtime-only: withAmbientStatus appends it to a throwaway
// per-request copy of history, never to the durable session log. It still
// round-trips through the canonical JSON union (marshalPart/unmarshalPart)
// like every other part, so a plugin or test that does build one, or a log
// that somehow carries one, survives persist/replay unchanged.
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
