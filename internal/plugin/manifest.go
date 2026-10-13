package plugin

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	pub "github.com/majorcontext/harness/plugin"
)

// validateManifest rejects a DenyOnFailure entry that is not a subscribed
// tool.execute.before hook.
func validateManifest(m pub.Manifest) error {
	for _, h := range m.DenyOnFailure {
		if h != pub.HookToolExecuteBefore {
			return fmt.Errorf("plugin %s: deny_on_failure is valid only for %s", m.Name, pub.HookToolExecuteBefore)
		}
		if !slices.Contains(m.Hooks, h) {
			return fmt.Errorf("plugin %s: deny_on_failure names %s, which hooks does not list", m.Name, h)
		}
	}
	return ValidateHookTimeouts(m)
}

// diffManifest names the first part of live that differs from m, or "" when none does.
func diffManifest(m, live pub.Manifest) string {
	switch {
	case m.Name != live.Name:
		return "name " + live.Name
	case m.ProtocolVersion != live.ProtocolVersion:
		return fmt.Sprintf("protocol version %d", live.ProtocolVersion)
	case !sameSet(m.Hooks, live.Hooks):
		return "hooks"
	case !sameSet(m.DenyOnFailure, live.DenyOnFailure):
		return "deny_on_failure"
	case !maps.Equal(m.HookTimeouts, live.HookTimeouts):
		return "hook_timeouts"
	case !sameTools(m.Tools, live.Tools):
		return "tools"
	}
	return ""
}

// sameManifest reports whether live describes the same plugin as m: name, protocol
// version, hooks and flagged hooks as sets, hook timeouts, and tools by definition.
func sameManifest(m, live pub.Manifest) bool { return diffManifest(m, live) == "" }

func sameTools(a, b []pub.ToolDef) bool {
	if len(a) != len(b) {
		return false
	}
	byName := make(map[string]pub.ToolDef, len(a))
	for _, d := range a {
		byName[d.Name] = d
	}
	if len(byName) != len(a) {
		return false
	}
	seen := make(map[string]bool, len(b))
	for _, d := range b {
		c, ok := byName[d.Name]
		if !ok || seen[d.Name] || c.Description != d.Description || !sameJSON(c.InputSchema, d.InputSchema) {
			return false
		}
		seen[d.Name] = true
	}
	return true
}

func sameSet(a, b []pub.Hook) bool {
	for _, h := range a {
		if !slices.Contains(b, h) {
			return false
		}
	}
	for _, h := range b {
		if !slices.Contains(a, h) {
			return false
		}
	}
	return true
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	errA, errB := json.Unmarshal(a, &x), json.Unmarshal(b, &y)
	if len(a) == 0 {
		errA = nil
	}
	if len(b) == 0 {
		errB = nil
	}
	if errA != nil || errB != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(x, y)
}
