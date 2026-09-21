package command

import "sort"

// Registry holds the resolved command set. NewRegistry builds it from a
// compiled-in table, so construction reads no disk.
type Registry struct {
	specs  []*Spec
	byName map[string]*Spec
}

func builtins() []*Spec {
	return []*Spec{
		{
			Name: "abort", Kind: KindControl, Op: OpAbort,
			Summary: "Stop the running turn", Category: CategorySession,
			Destructive: true, AvailableDuringTask: true, Source: "builtin",
		},
		{
			Name: "compact", Kind: KindControl, Op: OpCompact,
			Summary: "Summarize history and keep the last turns",
			ArgHint: "[keep_turns]", Category: CategorySession,
			Args:        []ArgSpec{{Name: "keep_turns", Type: ArgInt, Optional: true}},
			Destructive: true, Source: "builtin",
		},
		{
			Name: "goal", Kind: KindControl, Op: OpSetGoal,
			Summary: "Pursue a goal until its condition holds",
			ArgHint: "<condition>", Category: CategorySession,
			Args:                []ArgSpec{{Name: "condition", Type: ArgRest}},
			AvailableDuringTask: true,
			Source:              "builtin",
		},
		{
			Name: "goal-clear", Kind: KindControl, Op: OpClearGoal,
			Summary: "Stop pursuing the current goal", Category: CategorySession,
			AvailableDuringTask: true,
			Source:              "builtin",
		},
		{
			Name: "model", Kind: KindControl, Op: OpSetModel,
			Summary: "Change the session model", ArgHint: "<provider/model>",
			Args:     []ArgSpec{{Name: "model", Type: ArgString}},
			Category: CategoryModel, AvailableDuringTask: true, Source: "builtin",
		},
		{
			Name: "new", Aliases: []string{"clear"}, Kind: KindFrontend,
			Summary: "Start a new session", Category: CategoryFrontend,
			Destructive: true, Source: "builtin",
		},
		{
			Name: "processes", Kind: KindControl, Op: OpProcessList,
			Summary: "List managed processes", Category: CategoryInfo,
			AvailableDuringTask: true,
			Source:              "builtin",
		},
		{
			Name: "queue", Kind: KindControl, Op: OpQueueList,
			Summary: "Show queued prompts", Category: CategorySession,
			AvailableDuringTask: true,
			Source:              "builtin",
		},
		{
			Name: "queue-clear", Kind: KindControl, Op: OpQueueClear,
			Summary: "Drop every queued prompt", Category: CategorySession,
			Destructive: true, AvailableDuringTask: true, Source: "builtin",
		},
		{
			Name: "quit", Kind: KindFrontend,
			Summary: "Leave the session", Category: CategoryFrontend,
			Source: "builtin",
		},
		{
			Name: "resume", Kind: KindFrontend,
			Summary: "Talk to an existing session", ArgHint: "<session-id>",
			Args:     []ArgSpec{{Name: "session_id", Type: ArgString}},
			Category: CategoryFrontend, Source: "builtin",
		},
		{
			Name: "status", Kind: KindControl, Op: OpStatus,
			Summary: "Show the session state", Category: CategoryInfo,
			AvailableDuringTask: true,
			Source:              "builtin",
		},
		{
			Name: "thinking", Kind: KindControl, Op: OpSetThinking,
			Summary: "Set the reasoning effort", ArgHint: "<effort>",
			Args:     []ArgSpec{{Name: "effort", Type: ArgString}},
			Category: CategoryModel, AvailableDuringTask: true, Source: "builtin",
		},
		{
			Name: "tier", Kind: KindControl, Op: OpSetServiceTier,
			Summary: "Set the provider service tier", ArgHint: "<tier>",
			Args:     []ArgSpec{{Name: "service_tier", Type: ArgString}},
			Category: CategoryModel, AvailableDuringTask: true, Source: "builtin",
		},
	}
}

// NewRegistry returns the builtin registry, sorted by name.
func NewRegistry() *Registry {
	specs := builtins()
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	r := &Registry{specs: specs, byName: make(map[string]*Spec, len(specs)*2)}
	for _, s := range specs {
		r.byName[s.Name] = s
		for _, a := range s.Aliases {
			r.byName[a] = s
		}
	}
	return r
}

// Lookup returns the Spec a name or alias resolves to.
func (r *Registry) Lookup(name string) (*Spec, bool) {
	s, ok := r.byName[name]
	return s, ok
}

// All returns every Spec, sorted by name.
func (r *Registry) All() []*Spec {
	out := make([]*Spec, len(r.specs))
	copy(out, r.specs)
	return out
}
