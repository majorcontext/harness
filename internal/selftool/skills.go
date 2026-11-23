package selftool

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// skillsKind is the notice kind of the embedded skill update. A pin keeps the text in place, so the next request of
// the session extends this one and the system prompt stays as the session wrote it.
const skillsKind = "skills"

// SkillLister returns the embedded skills of a session.
type SkillLister func(ctx context.Context, session string) ([]prompt.Embedded, error)

// Skills tracks the embedded skills of one session. It is a turn.Source that gives load_skill: each model call reads
// the list again and load_skill loads the skills of that list. Notice builds the notice that holds the list when it
// differs from the list of the declared system prompt. The text depends only on the two lists, so a call that pins
// nothing loses nothing.
type Skills struct {
	list    SkillLister
	session string
	load    turn.Tool
	log     *slog.Logger

	mu sync.Mutex
	// now is the newest list that a refresh read. Until a refresh succeeds, the list of the declaration stands in.
	now       []prompt.Embedded
	refreshed bool
	declared  []prompt.Embedded
}

// NewSkills returns the tracker of a session.
func NewSkills(session string, list SkillLister, read SkillReader, log *slog.Logger) *Skills {
	s := &Skills{list: list, session: session, log: log}
	s.load = loadSkill{read, session, s.names}
	return s
}

// Toolset reads the list and gives load_skill and an empty skills notice, which the declaring source replaces. A
// failed read keeps the last list and warns.
func (s *Skills) Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, _ string) turn.Toolset {
	infos, err := s.list(ctx, s.session)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.log.Warn("harness: embedded skills not listed", "session", s.session, "err", err)
	} else {
		s.now, s.refreshed = prompt.ValidEmbedded(infos, nil), true
	}
	return turn.Toolset{Tools: turn.Restrict([]turn.Tool{s.load}, allowed), Notices: []turn.Notice{{Kind: skillsKind, At: len(history)}}}
}

// Notice returns the skills notice after the history of at messages, for a system prompt that lists declared.
func (s *Skills) Notice(declared []prompt.Embedded, at int) turn.Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := turn.Notice{Kind: skillsKind, At: at}
	s.declared = declared
	if !s.refreshed {
		return n
	}
	update := prompt.EmbeddedUpdate(s.now, removed(declared, s.now))
	if slices.Equal(s.now, declared) {
		n.Cleared = update
	} else {
		n.Text = update
	}
	return n
}

// removed names the skills of declared that now lacks.
func removed(declared, now []prompt.Embedded) []string {
	have := map[string]bool{}
	for _, e := range now {
		have[e.Name] = true
	}
	var out []string
	for _, e := range declared {
		if !have[e.Name] {
			out = append(out, e.Name)
		}
	}
	return out
}

// Current returns the embedded skills of the newest list, sorted by name. Before a refresh succeeds, it returns the
// list of the declaration.
func (s *Skills) Current() []prompt.Embedded {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refreshed {
		return slices.Clone(s.declared)
	}
	return slices.Clone(s.now)
}

func (s *Skills) names() []string {
	var names []string
	for _, e := range s.Current() {
		names = append(names, e.Name)
	}
	return names
}
