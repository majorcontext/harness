# Command

Read the root AGENTS.md.

- Keep this package a thin composition root. Move no engine behavior into handlers.
- Inject dependencies through options.
- Do not start a long-lived plugin process before its first hook or tool call.
- Start no MCP server or provider client before first use.
- Scan no skills or project instructions at `NewSession`.
- Keep plugin manifests and model metadata local and static on the hot path.
- Run the startup budget tests after a change to command initialization.
- This package resolves environment variables. The engine never reads them.
- Keep one decision point for each precedence rule. Preserve explicit zero, negative, and unset.
- Validate adapter-only fields against the adapter the entry builds. Fail loudly on unknown values.
- Keep run and serve wiring in parity for shared engine settings.
- The provider-map key is the model-reference family. Pass it into native Responses clients.
- Validate credentials on the first provider request, not in registry construction.
- Allow empty-token service only when `resolveUnauthenticated` proves loopback or gets the opt-in.
- Use `runtime/metrics` for GC pauses, not `runtime.ReadMemStats`.
- Never import `net/http/pprof`. Keep the default-mux regression test.
- Test flag and environment precedence as tables. Use no live provider.
