# Agent Skill

Read the root AGENTS.md. `engine/AGENTS.md` covers prompt integration.

- Keep two stages. `Load` validates frontmatter and retains no body.
- `Skill.Instructions` rereads the file and returns the body on demand.
- Discovery advertises only validated stage-one metadata.
- Never retain, inject, or interpret a body during discovery or startup.
- Keep the parser dependency-free and limited to the supported Agent Skills subset.
- Reject unknown top-level keys and unsupported nested structures.
- Require the skill name to match its parent directory. Keep rune-based limits.
- Sort discovered skills by name. Reject duplicate names across directories.
- A malformed `SKILL.md` fails discovery loudly.
- In `engine.Config`, an empty directory list disables discovery. A nil list keeps the default.
- Pin parser behavior with an `e2e/` contract row. Add no test here unless `testdata/test-exceptions.txt` lists the file.
