# Command

Read the root AGENTS.md.

- Keep this package a thin composition root. Move no engine behavior into handlers.
- Inject dependencies through options.
- Scan no skills or project instructions at `NewSession`.
- Keep plugin manifests and model metadata local and static on the hot path.
- After a change to command initialization, run the `Startup budget` step of `.github/workflows/ci.yml`.
- This package resolves environment variables. The engine never reads them.
- Keep one decision point for each precedence rule. Preserve explicit zero, negative, and unset.
- Keep run and serve wiring in parity for shared engine settings.
- The provider-map key is the model-reference family. Pass it into native Responses clients.
- Allow empty-token service only when `resolveUnauthenticated` proves loopback or gets the opt-in.
- Use `runtime/metrics` for GC pauses, not `runtime.ReadMemStats`.
- Test flag and environment precedence as tables. Use no live provider.
