# Copilot review instructions

Review each pull request against the architecture as a whole, not the diff alone.

- Read the installed `architecture-review` skill (`.agents/skills/architecture-review/SKILL.md`) first, then the harness checks in `.github/skills/harness-architecture-review/SKILL.md`, and answer their questions for every change. Cite the spec section (`docs/architecture.md`, section name) for each architecture finding.
- The binding spec is `docs/architecture.md`. The boxes spec is `docs/rearchitecture.md` in `meetneptune/boxes`. Read the section a change touches; do not rely on this file for its content.
- Read the root `AGENTS.md` and each scoped `AGENTS.md` that governs a changed path.
- A change that differs from the spec is a blocker unless it updates the spec in the same PR, as a decision of Andy, so that the spec describes the system as it is after the change. The spec holds no dated notes; the PR body states the decision.
- A PR body must cite the spec sections it implements.
- Do not flag pure style or formatting. Do not flag a violation that already existed and that the change does not touch.

## Structure

- Apply the installed `go-guide` skill (`.agents/skills/go-guide/SKILL.md`) and the `Structure` section of the harness checks to every change: imports, second definitions, package purpose and size, logic in `cmd/`, grab-bag names, and Go API design.
