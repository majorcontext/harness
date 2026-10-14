package modelref

import (
	"fmt"
	"slices"
)

// Effort is a unified reasoning-effort level for one model request. It is the
// canonical, provider-agnostic control the runtime carries. Each provider
// adapter maps it to that provider's own wire shape: Anthropic extended
// thinking (a token budget), OpenAI Responses reasoning.effort, and
// openai-compat top-level reasoning_effort.
//
// The zero value EffortUnset sends NO reasoning control at all — the provider
// runs its own default. EffortOff explicitly disables reasoning where a
// provider can (Anthropic: thinking disabled or no field); adapters that cannot disable
// reasoning treat EffortOff the same as EffortUnset (send no control).
//
// Effort does NOT police which model accepts which level. The model API
// backend clamps a level to the levels that the facts of the model list; the
// adapter sends the level it gets.
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
	// EffortXHigh is the level above high.
	EffortXHigh Effort = "xhigh"
	// EffortMax is the highest level.
	EffortMax Effort = "max"
)

// reasoningEfforts are the levels that request reasoning, lowest first.
var reasoningEfforts = []Effort{EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

func (e Effort) rank() int { return slices.Index(reasoningEfforts, e) }

// Cap returns e, or top when e is above top. A level that does not request
// reasoning is unchanged.
func (e Effort) Cap(top Effort) Effort {
	if e.rank() > top.rank() {
		return top
	}
	return e
}

// Fit returns the level that a request sends for e on a model that lists
// levels: ClampTo, or at most high when levels is empty.
func (e Effort) Fit(levels []string) Effort {
	if len(levels) == 0 {
		return e.Cap(EffortHigh)
	}
	return e.ClampTo(levels)
}

// ClampTo returns the level of levels that a request at e uses. A reasoning
// level is kept when listed, else it becomes the highest listed reasoning
// level below it, else the lowest listed reasoning level. It becomes off only
// when levels list no reasoning level and list off. Off and unset are
// unchanged.
func (e Effort) ClampTo(levels []string) Effort {
	if !e.Reasoning() || slices.Contains(levels, string(e)) {
		return e
	}
	best, lowest := -1, -1
	for _, l := range levels {
		o := slices.Index(reasoningEfforts, Effort(l))
		switch {
		case o < 0:
		case o < e.rank() && o > best:
			best = o
		case lowest < 0 || o < lowest:
			lowest = o
		}
	}
	switch {
	case best >= 0:
		return reasoningEfforts[best]
	case lowest >= 0:
		return reasoningEfforts[lowest]
	case slices.Contains(levels, string(EffortOff)):
		return EffortOff
	}
	return e
}

// ParseEffort validates s as an effort level. An empty string parses to
// EffortUnset. An unknown value errors, naming the valid set.
func ParseEffort(s string) (Effort, error) {
	e := Effort(s)
	switch e {
	case EffortUnset, EffortOff, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return e, nil
	default:
		return EffortUnset, fmt.Errorf("invalid effort %q (want one of: off, minimal, low, medium, high, xhigh, max, or empty)", s)
	}
}

// IsZero reports whether e is EffortUnset.
func (e Effort) IsZero() bool { return e == EffortUnset }

// Reasoning reports whether e requests any reasoning at all — true for the
// non-off levels, false for EffortUnset and EffortOff. Adapters use it to
// decide whether to emit a reasoning control at all.
func (e Effort) Reasoning() bool {
	return e.rank() >= 0
}

func (e Effort) String() string { return string(e) }
