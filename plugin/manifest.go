package plugin

import "time"

// Manifest returns the manifest of p: the hooks that are set, the flagged
// hooks, the hook timeouts in milliseconds, and the tools. A ToolBefore with a nil Run is not subscribed.
func (p *Plugin) Manifest() Manifest {
	m := Manifest{Name: p.Name, Version: p.Version, ProtocolVersion: ProtocolVersion}
	add := func(set bool, h Hook) {
		if set {
			m.Hooks = append(m.Hooks, h)
		}
	}
	before := p.ToolExecuteBefore != nil && p.ToolExecuteBefore.Run != nil
	add(p.Event != nil, HookEvent)
	add(p.ChatParams != nil, HookChatParams)
	add(p.ChatMessage != nil, HookChatMessage)
	add(p.SystemTransform != nil, HookSystemTransform)
	add(p.ShellEnv != nil, HookShellEnv)
	add(before, HookToolExecuteBefore)
	add(p.ToolExecuteAfter != nil, HookToolExecuteAfter)
	if before && p.ToolExecuteBefore.DenyOnFailure {
		m.DenyOnFailure = []Hook{HookToolExecuteBefore}
	}
	if len(p.HookTimeouts) > 0 {
		m.HookTimeouts = make(map[Hook]int, len(p.HookTimeouts))
		for h, d := range p.HookTimeouts {
			m.HookTimeouts[h] = int(d / time.Millisecond)
		}
	}
	for _, t := range p.Tools {
		m.Tools = append(m.Tools, t.Def)
	}
	return m
}
