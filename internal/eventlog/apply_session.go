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
	s.backends = map[string]string{}
	return nil
}

func (s *State) applySettings(e SettingsChanged) error {
	if e.Model != nil && *e.Model == "" {
		return illegal("settings.changed has an empty model")
	}
	if e.Model != nil {
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

func (s *State) applyCompaction(e CompactionApplied, seq uint64) error {
	if e.FromSeq == 0 || e.FromSeq > e.ToSeq || e.ToSeq >= seq {
		return illegal("compaction from_seq %d to_seq %d at seq %d", e.FromSeq, e.ToSeq, seq)
	}
	s.compaction = e
	s.compacted++
	s.usage = s.usage.Add(e.Usage)
	return nil
}

func (s *State) applyMeasured(e ContextMeasured) {
	s.usage = s.usage.Add(e.Usage)
	if e.Tokens > 0 {
		s.context = ContextMeasured{Tokens: e.Tokens, Window: e.Window, Source: e.Source}
	}
	if e.SubscriptionUsage != nil {
		s.subscribed = e.SubscriptionUsage
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
	if e.Backend == "" || e.BlobKey == "" {
		return illegal("backend.state needs a backend and a blob key")
	}
	s.backends[e.Backend] = e.BlobKey
	return nil
}

func (s *State) applyRetained(e ToolResultRetained) error {
	if e.Handle == "" || e.BlobKey == "" || slices.ContainsFunc(s.retained, func(r ToolResultRetained) bool { return r.Handle == e.Handle }) {
		return illegal("tool_result.retained needs a new handle and a blob key")
	}
	s.retained = append(s.retained, e)
	return nil
}
