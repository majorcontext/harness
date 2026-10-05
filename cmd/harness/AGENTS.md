# Command

Read the root AGENTS.md.

- Keep this package a thin composition root. `serve`, `run`, `sessions`, and `plugin probe` build one `harness.Runtime` from `harness.Options`; `serve` wraps its handler. Import neither `engine` nor `server`. Move no runtime behavior into it.
- Inject dependencies through options.
- Scan no skills or project instructions at `NewSession`.
- Keep plugin manifests and model metadata local and static on the hot path.
- After a change to command initialization, run the `Startup budget` step of `.github/workflows/ci.yml`.
- This package resolves environment variables: `serve` and `run` apply them with `config.ApplyEnv`, then their flags. The runtime reads none.
- Keep one decision point for each precedence rule. Preserve explicit zero, negative, and unset.
- The provider-map key is the model-reference family. Pass it into native Responses clients.
- Allow empty-token service only when `resolveUnauthenticated` proves loopback or gets the opt-in.
- Use `runtime/metrics` for GC pauses, not `runtime.ReadMemStats`.
- Pin flag and environment precedence with an `e2e/` contract row. Use no live provider. Add no test here unless `testdata/test-exceptions.txt` lists the file.
