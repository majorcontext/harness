package eventlog

import (
	"slices"
	"time"
)

func (s *State) applyCreated(e SessionCreated, t time.Time) error {
	if s.created {
		return illegal("session already created")
	}
	if e.Model == "" {
		return illegal("session.created has an empty model")
	}
	s.created = true
	s.parentID, s.agent, s.origin, s.model, s.settings, s.allowed = e.ParentID, e.Agent, e.Origin, e.Model, e.Settings, e.AllowedTools
	s.createdAt = t
	s.inputs = map[string]input{}
	s.turnIDs = map[string]bool{}
	s.children = map[string]Outcome{}
	s.backends = map[string]BackendChain{}
	return nil
}

func (s *State) applySettings(e SettingsChanged) error {
	if e.Model != nil && *e.Model == "" {
		return illegal("settings.changed has an empty model")
	}
	if e.Model != nil {
		if *e.Model != s.model {
			s.context.Window = 0
		}
		s.model = *e.Model
	}
	if e.Effort != nil {
		s.settings.Effort = *e.Effort
	}
	if e.ServiceTier != nil {
		s.settings.ServiceTier = *e.ServiceTier
	}
	return nil
}

func (s *State) applyCompaction(e CompactionApplied, seq uint64, at time.Time) error {
	if e.FromSeq == 0 || e.FromSeq > e.ToSeq || e.ToSeq >= seq {
		return illegal("compaction from_seq %d to_seq %d at seq %d", e.FromSeq, e.ToSeq, seq)
	}
	if s.compactedAt != 0 {
		s.folded = append(s.folded, s.summaryMessage())
	}
	s.compaction, s.compactedAt, s.compactedTime = e, seq, at
	s.compacted++
	s.usage = s.usage.Add(e.Usage)
	return nil
}

func (s *State) applyMeasured(e ContextMeasured) {
	s.usage = s.usage.Add(e.Usage)
	if e.Tokens > 0 || e.Source != "" {
		window, estimated := e.Window, e.WindowEstimated
		if window == 0 && e.Source != "" && e.Source == s.context.Source {
			window, estimated = s.context.Window, s.context.WindowEstimated
		}
		s.context = ContextMeasured{Tokens: e.Tokens, Window: window, WindowEstimated: estimated, Source: e.Source}
		s.measuredTurn = s.turnN
	}
	if e.SubscriptionUsage != nil {
		s.subscribed = e.SubscriptionUsage
	}
	if e.CostUSD != nil {
		sum := *e.CostUSD
		if s.cost != nil {
			sum += *s.cost
		}
		s.cost = &sum
	}
}

func (s *State) applyChildSpawned(e ChildSpawned) error {
	if o, ok := s.children[e.ChildID]; ok && o == "" || e.ChildID == "" {
		return illegal("child %q already spawned", e.ChildID)
	}
	s.children[e.ChildID] = ""
	return nil
}

func (s *State) applyChildSettled(e ChildSettled) error {
	outcome, ok := s.children[e.ChildID]
	switch {
	case !ok:
		return illegal("child %s is unknown", e.ChildID)
	case outcome != "":
		return illegal("child %s already settled %s", e.ChildID, outcome)
	case e.Outcome != OutcomeDone && e.Outcome != OutcomeFailed && e.Outcome != OutcomeCanceled:
		return illegal("child %s has outcome %q", e.ChildID, e.Outcome)
	}
	s.children[e.ChildID] = e.Outcome
	return nil
}

func (s *State) applyBackendState(e BackendState) error {
	if e.Backend == "" {
		return illegal("backend.state needs a backend")
	}
	if e.BlobKey != "" {
		if e.Head != nil || e.Chunk != "" || e.Restart || e.Entries != 0 || e.Sum != "" {
			return illegal("backend.state of one blob %s carries chain fields", e.BlobKey)
		}
		s.backends[e.Backend] = BackendChain{Legacy: e.BlobKey}
		return nil
	}
	if len(e.Head) == 0 {
		return illegal("backend.state of %s needs a head", e.Backend)
	}
	chain := s.backends[e.Backend]
	switch {
	case e.Restart:
		chain = BackendChain{}
	case chain.Legacy != "":
		return illegal("backend.state of %s continues a one-blob state", e.Backend)
	}
	if want := chain.Entries; e.Chunk == "" && e.Entries != want || e.Chunk != "" && e.Entries <= want {
		return illegal("backend.state of %s has %d entries after a chain of %d", e.Backend, e.Entries, want)
	}
	if e.Chunk != "" {
		chain.Chunks = append(slices.Clip(chain.Chunks), e.Chunk)
	}
	chain.Head, chain.Entries, chain.Sum = e.Head, e.Entries, e.Sum
	s.backends[e.Backend] = chain
	return nil
}

func (s *State) applyRetained(e ToolResultRetained) error {
	if e.Handle == "" || e.BlobKey == "" || slices.ContainsFunc(s.retained, func(r ToolResultRetained) bool { return r.Handle == e.Handle }) {
		return illegal("tool_result.retained needs a new handle and a blob key")
	}
	s.retained = append(s.retained, e)
	return nil
}
