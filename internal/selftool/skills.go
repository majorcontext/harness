package selftool

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// SkillLister returns the embedded skills of a session.
type SkillLister func(ctx context.Context, session string) ([]prompt.Embedded, error)

// Skills tracks the embedded skills of one session after the system prompt listed them. It is a turn.Source that
// gives load_skill: each model call reads the list again, and load_skill loads the skills of that list.
type Skills struct {
	list    SkillLister
	session string
	load    turn.Tool
	log     *slog.Logger

	mu  sync.Mutex
	now []prompt.Embedded
}

// NewSkills returns the tracker of a session whose system prompt listed start.
func NewSkills(start []prompt.Embedded, session string, list SkillLister, read SkillReader, log *slog.Logger) *Skills {
	s := &Skills{list: list, session: session, now: start, log: log}
	s.load = loadSkill{read, session, s.names}
	return s
}

// Toolset reads the list and gives load_skill. A failed read keeps the last list and warns.
func (s *Skills) Toolset(ctx context.Context, _ []protocol.LogMessage, allowed []string, _ string) turn.Toolset {
	infos, err := s.list(ctx, s.session)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.log.Warn("harness: embedded skills not listed", "session", s.session, "err", err)
	} else {
		s.now = prompt.ValidEmbedded(infos, nil)
	}
	return turn.Toolset{Tools: turn.Restrict([]turn.Tool{s.load}, allowed)}
}

// Current returns the embedded skills of the newest list, sorted by name.
func (s *Skills) Current() []prompt.Embedded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.now)
}

func (s *Skills) names() []string {
	names := map[string]bool{}
	for _, e := range s.Current() {
		names[e.Name] = true
	}
	return slices.Sorted(maps.Keys(names))
}
