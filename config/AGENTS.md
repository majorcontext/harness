# Config instructions

These rules apply to `config/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root.
Read `cmd/harness/AGENTS.md` for environment resolution and provider
construction.

## Config model

Keep configuration flat and cheap to parse. The config package must not perform
network access, start a subprocess, or initialize a provider.

Use pointer fields when zero and unset have different meanings. When a field's
documented contract distinguishes zero, false, empty, or a negative opt-out
from unset, preserve that distinction through merging. Slice fields may use a
different documented rule, such as an empty project value inheriting the user
value.

## Layering and merge

Load user config first and project config second. A project value overrides the
user value according to the field's documented merge rule.

`append_system_prompt` is the one additive key: the merged value is the user
segments followed by the project segments. Keep it additive. The user layer is
the platform's own config and the project layer is a cloned repository's file,
so an override rule would let a repository delete a platform segment. Do not
copy this shape to another key without the same argument.

Reject both Claude Code append-prompt options in `extra_args` when this key is
non-empty. The CLI would replace the managed value or reject the invocation.

Validate providers after merge and native-default application. A partial
project entry can become valid through its inherited fields.

Keep `LoadInfo` observational. It reports the effective source and summary; it
must not affect behavior or trigger a second read.

## Provider fields

Validate an adapter-specific field against the adapter that the entry builds.
Do not infer adapter identity from the map key alone.

Reject unreadable or unsupported values. Do not silently select a different
cache policy, request path, tool-loading mode, or durability mode.

## Environment boundary

The config package can define config values and merge semantics. The command
package owns operator environment variables and precedence. The engine must not
read those variables directly.

## Tests

Use table tests for merge and validation matrices. Cover absent, explicit zero,
negative, and malformed values. Assert the final merged config, not one layer
before defaults apply.
