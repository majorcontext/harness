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

// Skills tracks the embedded skills of one session after the system prompt listed them. It is a turn.Source that
// gives load_skill: each model call reads the list again, load_skill loads the skills of that list, and a notice
// holds the list when it differs from the list of the system prompt. The text depends only on the two lists, so a
// call that pins nothing loses nothing.
type Skills struct {
	list    SkillLister
	session string
	start   []prompt.Embedded
	load    turn.Tool
	log     *slog.Logger

	mu  sync.Mutex
	now []prompt.Embedded
}

// NewSkills returns the tracker of a session whose system prompt listed start.
func NewSkills(start []prompt.Embedded, session string, list SkillLister, read SkillReader, log *slog.Logger) *Skills {
	s := &Skills{list: list, session: session, start: start, now: start, log: log}
	s.load = loadSkill{read, session, s.names}
	return s
}

// Toolset reads the list and gives load_skill and the update as a notice. A failed read keeps the last list and warns.
func (s *Skills) Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, _ string) turn.Toolset {
	infos, err := s.list(ctx, s.session)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.log.Warn("harness: embedded skills not listed", "session", s.session, "err", err)
	} else {
		s.now = prompt.ValidEmbedded(infos, nil)
	}
	n := turn.Notice{Kind: skillsKind, At: len(history)}
	update := prompt.EmbeddedUpdate(s.now, s.removed())
	if slices.Equal(s.now, s.start) {
		n.Cleared = update
	} else {
		n.Text = update
	}
	return turn.Toolset{Tools: turn.Restrict([]turn.Tool{s.load}, allowed), Notices: []turn.Notice{n}}
}

// removed names the skills of the system prompt that the list lacks.
func (s *Skills) removed() []string {
	have := map[string]bool{}
	for _, e := range s.now {
		have[e.Name] = true
	}
	var out []string
	for _, e := range s.start {
		if !have[e.Name] {
			out = append(out, e.Name)
		}
	}
	return out
}

// Current returns the embedded skills of the newest list, sorted by name.
func (s *Skills) Current() []prompt.Embedded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.now)
}

func (s *Skills) names() []string {
	var names []string
	for _, e := range s.Current() {
		names = append(names, e.Name)
	}
	return names
}
