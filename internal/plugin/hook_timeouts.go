package plugin

import (
	"fmt"
	"maps"
	"slices"
	"time"

	pub "github.com/majorcontext/harness/plugin"
)

const maxHookTimeoutMS = 600000

// ValidateHookTimeouts rejects a hook timeout for the event hook or for a hook
// that hooks does not list, and a value outside 1 to 600000 ms.
func ValidateHookTimeouts(m pub.Manifest) error {
	for _, h := range slices.Sorted(maps.Keys(m.HookTimeouts)) {
		switch ms := m.HookTimeouts[h]; {
		case h == pub.HookEvent:
			return fmt.Errorf("plugin %s: hook_timeouts is not valid for event", m.Name)
		case !slices.Contains(m.Hooks, h):
			return fmt.Errorf("plugin %s: hook_timeouts names %s, which hooks does not list", m.Name, h)
		case ms < 1 || ms > maxHookTimeoutMS:
			return fmt.Errorf("plugin %s: hook_timeouts for %s must be from 1 to %d ms", m.Name, h, maxHookTimeoutMS)
		}
	}
	return nil
}

// hookTimeout is the dispatch deadline of hook: the manifest value, or def when the manifest has none.
func (inst *instance) hookTimeout(hook pub.Hook, def time.Duration) time.Duration {
	if ms, ok := inst.spec.Manifest.HookTimeouts[hook]; ok {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}
