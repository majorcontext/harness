package harness

import (
	"context"

	"github.com/majorcontext/harness/internal/prompt"
)

// SkillSource supplies the embedded skills of a session, for example from a database.
// The runtime calls List and Read concurrently, also for one session.
type SkillSource interface {
	// List returns the skills of a session.
	List(ctx context.Context, session string) ([]SkillInfo, error)
	// Read returns one file of a skill: "SKILL.md" or a slash-separated path relative to the skill root.
	Read(ctx context.Context, session, name, file string) ([]byte, error)
}

// SkillInfo names an embedded skill.
type SkillInfo struct {
	Name        string
	Description string
}

// listSkills returns the embedded skills of session id. A failed List gives none.
func (r *Runtime) listSkills(ctx context.Context, id string) []prompt.Embedded {
	if r.skills == nil {
		return nil
	}
	infos, err := r.skills.List(ctx, id)
	if err != nil {
		r.log.Warn("harness: embedded skills not listed", "session", id, "err", err)
		return nil
	}
	out := make([]prompt.Embedded, len(infos))
	for i, s := range infos {
		out[i] = prompt.Embedded{Name: s.Name, Description: s.Description}
	}
	return out
}
