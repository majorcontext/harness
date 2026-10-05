# Model metadata

Read the root AGENTS.md. `engine/AGENTS.md` owns context-window policy.

- Keep metadata static and deterministic. No network or background refresh.
- Never edit `context_windows_gen.go`. Run `go generate ./modelmeta/`.
- Put entries that models.dev lacks in `overrides.json`.
- Run the generator only by hand or in the `modelmeta-refresh` workflow.
- Keep zero unavailable for non-chat models. Zero means unknown.
- Keep provider-family matching explicit. Preserve dated variants and aliases.
- An unknown model returns no window. The engine owns refusal policy.
- Guess no capability from a model-name substring.
- `SupportsToolSearch` uses an explicit first-party Anthropic allowlist.
- Return false for other families and Bedrock-style Anthropic refs.
- Keep Bifrost namespace stripping aligned with context-window lookup.
- Pin refs, variants, near misses, unknown families, and tool-search refusals with an `e2e/` contract row. Add no test here unless `testdata/test-exceptions.txt` lists the file.
