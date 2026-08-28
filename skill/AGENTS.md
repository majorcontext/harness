# Agent Skill instructions

These rules apply to `skill/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root.
Read `engine/AGENTS.md` for prompt integration.

## Progressive disclosure

Keep the two-stage contract.

- `Load` validates frontmatter without retaining or interpreting the Markdown
  body.
- `Skill.Instructions` reads the file again and returns the body on demand.
- Engine discovery advertises only validated stage-one metadata.

Do not retain, inject, or interpret skill bodies during discovery or session
startup.

## Frontmatter parser

Keep the parser dependency-free and limited to the supported Agent Skills
subset. Reject unknown top-level keys and unsupported nested structures. Do not
silently interpret general YAML features.

Require the skill name to match its parent directory. Preserve rune-based field
limits.

## Discovery

Sort discovered skills by name. Reject duplicate names across configured
directories. A malformed `SKILL.md` fails discovery loudly.

In the resolved `engine.Config`, an explicit empty directory list disables
discovery and a nil list keeps the project default. Preserve `config` package
layering semantics before that resolved value reaches the engine.

## Tests

Keep parser tables and fuzz coverage for frontmatter boundaries. Test stage-one
reads separately from body reads. Assert deterministic discovery order and
duplicate failure.
