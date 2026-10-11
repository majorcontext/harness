# Copilot review instructions

Review each pull request against the architecture as a whole, not the diff alone.

- Read `.github/skills/architecture-review/SKILL.md` first and answer its questions for every change. Cite the spec line (`docs/architecture.md:<line>`) for each architecture finding.
- The binding spec is `docs/architecture.md`. The boxes spec is `docs/rearchitecture.md` in `meetneptune/boxes`. Read the section a change touches; do not rely on this file for its content.
- Read the root `AGENTS.md` and each scoped `AGENTS.md` that governs a changed path.
- A change that differs from the spec is a blocker unless it updates the spec in the same PR and cites a dated Andy decision.
- A PR body must cite the spec lines it implements.
- Do not flag pure style or formatting. Do not flag a violation that already existed and that the change does not touch.

## Structure

- Check each new import against `Internal packages` in `docs/architecture.md` and the `depguard` rules in `.golangci.yml`.
- Flag a type or concept defined a second time; each record has one owner.
- Each package has one purpose; a file over about 1,000 lines or a function over about 150 lines needs a stated reason.
- Flag logic in `cmd/` that belongs in `internal/`.
- Flag `util`, `common`, or `helpers` packages and files named `types.go` or `interfaces.go`.
- Apply the Go API rules in the `architecture-review` skill `Structure` section and the root `AGENTS.md` `Code` section: context first, consumer-declared interfaces, documented concurrency and ownership, errors matchable with `errors.Is`/`errors.As`, `iter.Seq` for streams, no stutter.
