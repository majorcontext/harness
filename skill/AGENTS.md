# Agent Skill

Read the root AGENTS.md.

- Keep two stages. `Load` validates frontmatter and retains no body.
- `Skill.Instructions` rereads the file and returns the body on demand.
- The skill list advertises only validated stage-one metadata.
- Never retain, inject, or interpret a body while listing or at startup.
- Keep the parser dependency-free and limited to the supported Agent Skills subset.
- Reject unknown top-level keys and unsupported nested structures.
- Require the skill name to match its parent directory. Keep rune-based limits.
- `Load` rejects a malformed `SKILL.md` with an error that names the file.
- Name uniqueness and ordering belong to the caller (`internal/prompt`).
- In `config.Config.SkillsDirs`, an empty list disables discovery. A nil list keeps the default.
- Pin parser behavior with an `e2e/` contract row. Add no test here unless `testdata/test-exceptions.txt` lists the file.
