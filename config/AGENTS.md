# Config

Read the root AGENTS.md. `cmd/harness/AGENTS.md` covers environment resolution.

- Keep config flat and cheap. No network, subprocess, or provider initialization.
- Import only the standard library.
- `Validate` is the one rule set. Add a rule there, never at a call site.
- `Defaults` is the one defaults table. An accessor reads it for an unset key.
- `ApplyEnv` reads `HARNESS_<KEY>` for top-level scalar keys only.
- Use pointer fields when zero and unset differ. Preserve that distinction through merging.
- Load user config first, project config second. A project value overrides by the field's merge rule.
- `append_system_prompt` is the one additive key: user segments, then project segments.
- `plugins` is the second additive key: project plugins, then user plugins. The last plugin has the last word on a hook chain, so the most trusted plugins run last. A project config then cannot remove a user guard or rewrite a call after it.
- A plugin name in both files is an error that names the plugin and both files. `LoadProject` checks it.
- Do not copy that additive shape to another key without the same trust argument.
- Reject both Claude Code append-prompt options in `extra_args` when `append_system_prompt` is non-empty.
- Validate providers once, on the merged config.
- Validate an adapter field against the adapter the entry builds, not the map key.
- Reject unreadable or unsupported values. Never silently pick another policy.
- Use table tests for merge and validation. Cover absent, zero, negative, and malformed values.
- Assert the final merged config.
