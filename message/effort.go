package message

import "fmt"

// Effort is a unified reasoning-effort level for one model request. It is the
// canonical, provider-agnostic control the engine carries. Each provider
// adapter maps it to that provider's own wire shape: Anthropic extended
// thinking (a token budget), OpenAI Responses reasoning.effort, and
// openai-compat top-level reasoning_effort.
//
// The zero value EffortUnset sends NO reasoning control at all — the provider
// runs its own default. EffortOff explicitly disables reasoning where a
// provider can (Anthropic: no thinking block); adapters that cannot disable
// reasoning treat EffortOff the same as EffortUnset (send no control).
//
// Effort does NOT police which model accepts which level. That is a
// provider-and-model fact the engine cannot know from the model ref alone —
// the adapter sends the requested level and the provider is the final judge.
// A caller that must gate levels per model (a dashboard picker) holds its own
// mapping; see the boxes bifrost catalog.
type Effort string

const (
	// EffortUnset is the zero value: no reasoning control is sent.
	EffortUnset Effort = ""
	// EffortOff explicitly disables reasoning where the provider supports it.
	EffortOff Effort = "off"
	// EffortMinimal is the lowest non-off level (OpenAI GPT-5 floor).
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
)

// ParseEffort validates s as an effort level. An empty string parses to
// EffortUnset. An unknown value errors, naming the valid set.
func ParseEffort(s string) (Effort, error) {
	e := Effort(s)
	switch e {
	case EffortUnset, EffortOff, EffortMinimal, EffortLow, EffortMedium, EffortHigh:
		return e, nil
	default:
		return EffortUnset, fmt.Errorf("message: invalid effort %q (want one of: off, minimal, low, medium, high, or empty)", s)
	}
}

// IsZero reports whether e is EffortUnset.
func (e Effort) IsZero() bool { return e == EffortUnset }

// Reasoning reports whether e requests any reasoning at all — true for the
// four non-off levels, false for EffortUnset and EffortOff. Adapters use it to
// decide whether to emit a reasoning control at all.
func (e Effort) Reasoning() bool {
	switch e {
	case EffortMinimal, EffortLow, EffortMedium, EffortHigh:
		return true
	default:
		return false
	}
}

func (e Effort) String() string { return string(e) }
