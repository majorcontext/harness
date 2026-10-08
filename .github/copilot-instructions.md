# Copilot review instructions

Review each pull request against the architecture as a whole, not the diff alone.

- Read `.github/skills/architecture-review/SKILL.md` first and answer its questions for every change. Cite the spec line (`docs/architecture.md:<line>`) for each architecture finding.
- The binding spec is `docs/architecture.md`. The boxes spec is `docs/rearchitecture.md` in `meetneptune/boxes`. Read the section a change touches; do not rely on this file for its content.
- Read the root `AGENTS.md` and each scoped `AGENTS.md` that governs a changed path.
- A change that differs from the spec is a blocker unless it updates the spec in the same PR and cites a dated Andy decision.
- A PR body must cite the spec lines it implements.
- Do not flag pure style or formatting. Do not flag a violation that already existed and that the change does not touch.
